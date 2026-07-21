// csrpc は cs-rpc サーバを叩く CLI（設計書 01 の5章）。
//
// 使い方:
//
//	csrpc call <method> [--param k=v ...] [--params-json '{...}'] [--idempotent]
//	csrpc ping
//	csrpc methods
//
// 終了コード: 0=成功 / 1=業務エラー(RPCError) / 2=トランスポート・使用法エラー。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"csrpc/internal/rpc"
)

const (
	exitOK        = 0
	exitRPCError  = 1
	exitTransport = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitTransport
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "call":
		return cmdCall(rest)
	case "ping":
		return cmdPing(rest)
	case "methods":
		return cmdMethods(rest)
	case "worker":
		return cmdWorker(rest)
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", sub)
		usage()
		return exitTransport
	}
}

// commonFlags は全サブコマンド共通のフラグを FlagSet に登録する。
type commonFlags struct {
	endpoint string
	timeout  time.Duration
	retries  int
	headers  keyValue
	output   string
	logLevel string
}

func registerCommon(fs *flag.FlagSet) *commonFlags {
	def := rpc.DefaultOptions()
	c := &commonFlags{headers: keyValue{}}
	fs.StringVar(&c.endpoint, "endpoint", envOr("CSRPC_ENDPOINT", def.Endpoint), "server RPC endpoint")
	fs.DurationVar(&c.timeout, "timeout", envDurOr("CSRPC_TIMEOUT", def.Timeout), "request timeout")
	fs.IntVar(&c.retries, "retries", envIntOr("CSRPC_RETRIES", def.Retries), "max retries for idempotent calls")
	fs.Var(&c.headers, "header", "extra header k=v (repeatable)")
	fs.StringVar(&c.output, "output", "json", "output format: json|raw")
	fs.StringVar(&c.logLevel, "log-level", envOr("CSRPC_LOG", "info"), "log level: debug|info")
	return c
}

func (c *commonFlags) newClient() (*rpc.Client, error) {
	opts := rpc.DefaultOptions()
	opts.Endpoint = c.endpoint
	opts.Timeout = c.timeout
	opts.Retries = c.retries
	opts.Headers = c.headers
	if c.logLevel == "debug" {
		opts.Logger = log.New(os.Stderr, "[csrpc] ", log.LstdFlags)
	}
	return rpc.New(opts)
}

func cmdCall(args []string) int {
	fs := newFlagSet("call")
	common := registerCommon(fs)
	var paramsJSON string
	params := keyValue{}
	var idempotent bool
	fs.Var(&params, "param", "param k=v as string (repeatable)")
	fs.StringVar(&paramsJSON, "params-json", "", "params as raw JSON object")
	fs.BoolVar(&idempotent, "idempotent", false, "allow retry on transport error")

	// Go の flag は最初の非フラグ引数で解析を止めるため、method を先に取り出してから
	// 残りのフラグを解析する。これで `call <method> --param ...` の順序を許容する。
	var method string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		method, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return exitTransport
	}
	if method == "" && fs.NArg() > 0 { // フラグの後ろに置かれた場合の保険
		method = fs.Arg(0)
	}
	if method == "" {
		fmt.Fprintln(os.Stderr, "usage: csrpc call <method> [--param k=v] [--params-json '{...}']")
		return exitTransport
	}

	var reqParams any
	switch {
	case paramsJSON != "":
		reqParams = json.RawMessage(paramsJSON)
	case len(params) > 0:
		reqParams = map[string]string(params)
	default:
		reqParams = nil
	}

	client, err := common.newClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitTransport
	}
	ctx, cancel := context.WithTimeout(context.Background(), common.timeout)
	defer cancel()

	var result json.RawMessage
	var callOpts []rpc.CallOption
	if idempotent {
		callOpts = append(callOpts, rpc.Idempotent())
	}
	if err := client.Call(ctx, method, reqParams, &result, callOpts...); err != nil {
		return reportError(err)
	}
	printResult(result, common.output)
	return exitOK
}

func cmdPing(args []string) int {
	fs := newFlagSet("ping")
	common := registerCommon(fs)
	if err := fs.Parse(args); err != nil {
		return exitTransport
	}
	client, err := common.newClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitTransport
	}
	ctx, cancel := context.WithTimeout(context.Background(), common.timeout)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return reportError(err)
	}
	fmt.Println("ok")
	return exitOK
}

func cmdMethods(args []string) int {
	fs := newFlagSet("methods")
	common := registerCommon(fs)
	if err := fs.Parse(args); err != nil {
		return exitTransport
	}
	client, err := common.newClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitTransport
	}
	ctx, cancel := context.WithTimeout(context.Background(), common.timeout)
	defer cancel()
	raw, err := client.Methods(ctx)
	if err != nil {
		return reportError(err)
	}
	printResult(raw, common.output)
	return exitOK
}

func reportError(err error) int {
	var rpcErr *rpc.RPCError
	if errors.As(err, &rpcErr) {
		fmt.Fprintln(os.Stderr, rpcErr.Error())
		return exitRPCError
	}
	fmt.Fprintln(os.Stderr, err.Error())
	return exitTransport
}

func printResult(raw json.RawMessage, format string) {
	if len(raw) == 0 {
		return
	}
	if format == "raw" {
		fmt.Println(string(raw))
		return
	}
	var buf strings.Builder
	if err := indentJSON(&buf, raw); err != nil {
		fmt.Println(string(raw)) // フォールバック
		return
	}
	fmt.Println(buf.String())
}

func indentJSON(w io.Writer, raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func usage() {
	fmt.Fprint(os.Stderr, `csrpc - cs-rpc client

Usage:
  csrpc call <method> [--param k=v ...] [--params-json '{...}'] [--idempotent]
  csrpc ping
  csrpc methods
  csrpc worker [--gui 127.0.0.1:8787] [--name NAME] [--poll 500ms] [--no-open]

Common flags:
  --endpoint URL     (env CSRPC_ENDPOINT, default http://127.0.0.1:8080/rpc)
  --timeout DUR      (env CSRPC_TIMEOUT, default 30s)
  --retries N        (env CSRPC_RETRIES, default 2)
  --header k=v       extra header, repeatable
  --output json|raw  (default json)
  --log-level LEVEL  debug|info (env CSRPC_LOG)
`)
}
