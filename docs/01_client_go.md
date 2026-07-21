# クライアント設計書 — cs-rpc (Go)

Windows / Linux で単一バイナリとして動作する RPC クライアント。
CLI としても、他プログラムから使うライブラリとしても使えるようにする。

- 関連: [全体設計書](00_overview.md) / [サーバ設計書](02_server_python.md)

---

## 1. 役割と設計方針

### 1.1 役割
- ユーザ/呼び出し元からのコマンドを **JSON-RPC 2.0 リクエスト**に変換して送信。
- HTTP でサーバへ POST し、レスポンスを解釈して結果 or エラーを返す。
- タイムアウト・リトライ・相関ID採番などの横断処理を担当。

### 1.2 方針
- **依存最小・単一バイナリ**: 標準ライブラリ中心（`net/http`, `encoding/json`）。
  CGO 無効（`CGO_ENABLED=0`）で完全静的リンク → Windows/Linux で単体配布。
- **ライブラリ / CLI の分離**: 中核は `internal/rpc` のクライアントライブラリ。
  `cmd/csrpc` はその薄い CLI ラッパ。
- **クロスプラットフォーム配慮**: パスは `path/filepath`、改行や端末依存を避ける。
  設定ファイルの場所も OS 毎の慣習（後述）に従う。

---

## 2. コンポーネント構成

```
client/
├── go.mod
├── cmd/
│   └── csrpc/
│       └── main.go          # CLI: 引数解析 → Client 呼び出し → 出力
└── internal/
    └── rpc/
        ├── client.go        # Client 本体（Call/Notify）
        ├── protocol.go      # Request/Response/Error 構造体
        ├── options.go       # 設定（URL, timeout, retry, headers）
        ├── transport.go     # HTTP 送受信・エンコード/デコード
        └── errors.go        # エラー型（RPCError, TransportError）
```

### 2.1 主要型（抜粋・イメージ）
```go
// protocol.go
type Request struct {
    JSONRPC string      `json:"jsonrpc"`          // "2.0"
    ID      string      `json:"id,omitempty"`     // UUID。Notifyでは空
    Method  string      `json:"method"`
    Params  any         `json:"params,omitempty"`
}

type Response struct {
    JSONRPC string          `json:"jsonrpc"`
    ID      string          `json:"id"`
    Result  json.RawMessage `json:"result,omitempty"`
    Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
    Code    int             `json:"code"`
    Message string          `json:"message"`
    Data    json.RawMessage `json:"data,omitempty"`
}
```

```go
// client.go
type Client struct {
    endpoint string
    http     *http.Client
    opts     Options
}

// Call は同期 RPC。result にサーバの result をアンマーシャルする。
func (c *Client) Call(ctx context.Context, method string, params any, result any) error

// Notify は応答不要の通知（id なし）。
func (c *Client) Notify(ctx context.Context, method string, params any) error
```

- `Call` は `RPCError`（サーバが返した業務エラー）と
  `TransportError`（接続失敗・タイムアウト・JSON破損）を区別して返す。
  呼び出し側は `errors.As` で判別しリトライ可否を判断する。

---

## 3. 設定（Options）

優先順位: **コマンドライン引数 > 環境変数 > 設定ファイル > 既定値**。

| 項目 | フラグ | 環境変数 | 既定値 |
| --- | --- | --- | --- |
| サーバURL | `--endpoint` | `CSRPC_ENDPOINT` | `http://127.0.0.1:8080/rpc` |
| タイムアウト | `--timeout` | `CSRPC_TIMEOUT` | `30s` |
| リトライ回数 | `--retries` | `CSRPC_RETRIES` | `2` |
| 追加ヘッダ | `--header k=v`（複数可） | — | なし |
| 出力形式 | `--output json\|raw` | — | `json` |
| ログレベル | `--log-level` | `CSRPC_LOG` | `info` |

- 設定ファイル位置（任意）: Linux は `$XDG_CONFIG_HOME/csrpc/config.toml`
  （既定 `~/.config/...`）、Windows は `%APPDATA%\csrpc\config.toml`。
  `os.UserConfigDir()` を使えば OS 差を吸収できる。
- `--header` は将来の認証トークン注入（`Authorization: Bearer ...` 等）に流用できる。

---

## 4. 処理フロー

```
main.go
  1. 引数/環境変数/設定ファイルを解決 → Options
  2. rpc.New(opts) で Client 生成
  3. サブコマンドに応じて params を組み立て
  4. client.Call(ctx, method, params, &result)
  5. 結果を --output 形式で標準出力へ、エラーは stderr + 終了コード
```

Client.Call 内部:
```
  a. id = uuid.New()
  b. Request を JSON エンコード
  c. context のタイムアウト付きで POST
  d. ステータス/ボディを検査
       - 2xx かつ result → result にデコードして nil
       - 2xx かつ error  → RPCError を返す
       - 4xx/5xx / body破損 / 接続失敗 → TransportError
  e. TransportError かつ 冪等 かつ retries 残 → 指数バックオフで再送
```

### 4.1 リトライ方針
- **冪等なメソッドのみ**再送（既定で `Call` は再送しない安全側。`--retries` と、
  メソッド定義側の「冪等フラグ」を掛け合わせて判断）。
- 対象: 接続拒否・タイムアウト・5xx・503。対象外: 4xx・業務 `error`。
- バックオフ: `base * 2^n`（既定 base=200ms）＋ジッタ。上限は `--timeout` 全体。

### 4.2 タイムアウト
- `context.WithTimeout` を Call ごとに設定。`http.Client.Timeout` ではなく
  context で制御し、リトライ全体の総時間も別 context で上限を設ける。

---

## 5. CLI インターフェース（例）

```
csrpc call <method> [--param k=v ...] [--params-json '{...}']
csrpc ping                # /healthz を叩いて疎通確認
csrpc methods             # /rpc/methods で登録メソッド一覧
```

例:
```
$ csrpc call echo --param message=hello
{ "message": "hello" }

$ csrpc call job.run --params-json '{"name":"build","args":["-v"]}'
```

- `--param k=v` は簡易指定（値は文字列）。複雑な型は `--params-json` を使用。
- 終了コード: `0`=成功 / `1`=業務エラー(RPCError) / `2`=トランスポート/使用法エラー。
  スクリプトからの利用時に成否を判定しやすくする。

---

## 6. エラーハンドリング設計

| 種別 | Go 型 | 例 | 呼び出し側の扱い |
| --- | --- | --- | --- |
| 業務エラー | `*RPCError` | method not found, invalid params | メッセージ表示・非再送 |
| トランスポート | `*TransportError` | connection refused, timeout, 5xx | 再送候補 |
| 使用法エラー | 通常の `error` | 不正な引数、URL 未設定 | 即時終了・ヘルプ表示 |

- ログには常に `id`・`method`・所要時間を出力し、サーバログと突合可能にする。

---

## 7. クロスプラットフォーム対応

- ビルド:
  ```
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o csrpc.exe ./cmd/csrpc
  CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o csrpc     ./cmd/csrpc
  ```
- パス操作は `path/filepath`、設定ディレクトリは `os.UserConfigDir()`。
- 端末色付けは Windows で無効化可能に（`--no-color` / 非TTY時は自動オフ）。
- 改行・文字コードに依存しない（入出力は UTF-8、JSON はバイト列として扱う）。

---

## 8. テスト方針
- `internal/rpc`: `httptest.Server` によるモックで正常/各種エラー/タイムアウト/
  リトライ挙動を検証。
- プロトコル契約: 全体設計 3章の JSON 例をフィクスチャに用いた round-trip テスト。
- CLI: 引数解析と終了コードのテーブルドリブンテスト。
- CI で `GOOS=windows` / `GOOS=linux` の両ビルドを実施。

---

## 9. 拡張ポイント
- **認証**: `Options.Headers` / `--header` にトークンを載せるだけで対応可能。
  さらに `RoundTripper` を差し替えれば署名・自動リフレッシュも実装できる。
- **TLS**: `--endpoint https://...` と `http.Client.Transport.TLSClientConfig` で対応。
- **新コマンド**: サーバ側でメソッドを増やすだけ。クライアントは汎用 `call` で叩ける。
  よく使うものは専用サブコマンドを追加してもよい。
