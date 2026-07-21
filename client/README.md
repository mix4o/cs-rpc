# cs-rpc client (Go)

Windows / Linux で単一バイナリとして動作する JSON-RPC 2.0 クライアント。
設計は [../docs/01_client_go.md](../docs/01_client_go.md) を参照。

## ビルド
```bash
cd client
go build -o csrpc ./cmd/csrpc            # 実行中の OS 向け（GUI はブラウザ表示）

# クロスコンパイル（静的単一バイナリ・CGO 不要）
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o csrpc.exe ./cmd/csrpc
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o csrpc     ./cmd/csrpc
```
`worker` の GUI をネイティブなデスクトップウィンドウで表示したい場合は
`-tags webview` でビルドする（CGO 必須・OS 別依存あり）。
→ [worker のネイティブウィンドウ表示](#ネイティブウィンドウ表示-tags-webview) を参照。

## 使い方
サブコマンドが先、フラグは後ろ（git スタイル）。

```bash
csrpc call <method> [--param k=v ...] [--params-json '{...}'] [--idempotent]
csrpc ping
csrpc methods
csrpc worker [--gui 127.0.0.1:8787] [--name NAME] [--poll 500ms]
```

例:
```bash
export CSRPC_ENDPOINT=http://127.0.0.1:8080/rpc

csrpc ping
csrpc call echo --param message=hello
csrpc call math.add --params-json '{"a":2,"b":40}'
csrpc call sys.info --output raw
csrpc methods
```

## worker サブコマンド（GUI 付きデモワーカ）
サーバの制御キューからコマンドを受け取り、**クライアント側で実行**して結果を報告する。
起動すると **ローカル Web GUI**（既定 `http://127.0.0.1:8787`）を開き、次の2ペインを表示:

- **左ペイン**: サーバから受領したコマンドとその実行結果（running→done/error、所要時間）
- **右ペイン**: クライアント処理のデバッグログ（lease / 実行 / 完了報告）

いずれも追記されるたびに自動スクロールし、上へスクロールすると追従を止めて遡れる
（新着時は「↓ new」ボタン表示）。更新は SSE（Server-Sent Events）でリアルタイム配信。

```bash
# サーバを起動しておく（別端末）
CSRPC_ENDPOINT=http://127.0.0.1:8080/rpc csrpc worker --name my-worker
# → ブラウザで http://127.0.0.1:8787 が開く
```

起動時にサーバの autorun を OFF にして実行を引き取る（`--take-over`, 既定 ON）。
コントロールページ（サーバ側 `GET /`）からコマンドを挿入すると、このワーカが処理して
GUI に流れる。`sys.info` はワーカ機の OS/アーキ（`executedOn:"client"`）を返すので、
「サーバが指示し、クライアントが実行した」ことが可視化できる。

### ネイティブウィンドウ表示（`-tags webview`）
研修用途で「サーバ（ブラウザのタブ）」と「クライアント（独立したデスクトップ
ウィンドウ）」を一目で区別したい場合は、ネイティブウィンドウ版をビルドする。
OS のシステム webview にローカル GUI を表示し、タイトルは `cs-rpc CLIENT — worker: <名前>`、
画面上部に teal のバンドと **CLIENT** バッジが付く。

> ビルドタグ無し（既定）は CGO 不要でブラウザにフォールバック（全 OS 共通の単一バイナリ）。
> `-tags webview` は **CGO 必須**で、OS ごとに下記のランタイム/開発ライブラリが必要。
> クロスコンパイルは難しいため、原則 **各ターゲット OS 上でビルド**（または各 OS で
> 作った実行ファイルを配布）する。

```bash
# Windows（PowerShell）— 実行時は WebView2 ランタイムが必要（Win10/11 は標準搭載）
#   ビルドには C コンパイラ（例: MSYS2 の mingw-w64 gcc）が必要
$env:CGO_ENABLED=1 ; go build -tags webview -ldflags "-H=windowsgui" -o csrpc.exe ./cmd/csrpc
#   -H=windowsgui はコンソール窓を出さずウィンドウのみ表示する指定

# Linux — webkit2gtk 開発ライブラリが必要
#   Ubuntu 22.04: sudo apt install gcc pkg-config libwebkit2gtk-4.0-dev  → そのままビルド可
#   Ubuntu 24.04: 4.0 が無く 4.1 のみ。webview_go(現状ピン)は 4.0 を要求するため
#                 pkg-config シムで 4.0→4.1 を橋渡しする（API 互換）:
#     sudo apt install gcc pkg-config libwebkit2gtk-4.1-dev
#     mkdir -p /tmp/pcshim
#     printf 'Name: webkit2gtk-4.0\nDescription: shim\nVersion: 2.44\nRequires: webkit2gtk-4.1\n' > /tmp/pcshim/webkit2gtk-4.0.pc
#     export PKG_CONFIG_PATH=/tmp/pcshim:$(pkg-config --variable pc_path pkg-config)
CGO_ENABLED=1 go build -tags webview -o csrpc ./cmd/csrpc
#   ※ 実行には X（デスクトップ）が必要。ヘッドレス検証は `xvfb-run -a ./csrpc worker ...`

# macOS（参考）— 追加依存なし（WKWebView）
CGO_ENABLED=1 go build -tags webview -o csrpc ./cmd/csrpc
```
起動は同じ（`csrpc worker ...`）。ネイティブ版では `--open` は無視され、
ウィンドウを閉じるとワーカも終了する。

| フラグ | 既定 | 説明 |
| --- | --- | --- |
| `--gui addr` | `127.0.0.1:8787` | GUI の待受アドレス（空文字で GUI 無効） |
| `--name` | ホスト名 | サーバへ報告するワーカ名 |
| `--poll` | `500ms` | lease のポーリング間隔 |
| `--take-over` | `true` | 起動時にサーバ autorun を無効化 |
| `--open` | `true` | 既定ブラウザで GUI を開く |

ローカル実行できるコマンド: `echo` / `math.add` / `math.div` / `sys.info` / `sys.time` /
`demo.sleep`（running 状態を見せるデモ用）/ `find` / `exec`。追加は
`internal/worker/handlers.go`。ワーカは起動時に自分の対応メソッドをサーバへ申告するので、
コントロールページのメソッド選択に自動で現れる。

### exec（外部プログラム実行）と allowlist ⚠️
`exec`（`{program, args?, wait?}`）はクライアント上でプログラムを実行する。`wait:false` は
起動して即完了（`calc.exe` 等）、`wait:true` は完了まで待ち stdout/終了コードを返す。

これは実質リモートコード実行なので、**既定では無効**。実行するには**ワーカ側の環境変数
`CSRPC_EXEC_ALLOW`** に許可プログラム名を列挙する（そこにあるものだけ実行可）:

```powershell
# Windows (PowerShell): calc と notepad だけ許可して worker 起動
$env:CSRPC_EXEC_ALLOW = "calc,notepad"
$env:CSRPC_ENDPOINT   = "http://192.168.1.180:8080/rpc"
.\csrpc-native.exe worker --name win-pc
```
未設定なら `exec` は `error 1003`（無効）、allowlist 外は `error 1002`（不許可）で拒否される。
信頼できるネットワーク限定で使うこと。

### 長時間コマンド（find）と進捗・キャンセル
`find`（params 例: `{"path":"/etc","name":"*.conf"}`）は時間がかかり得るため、
非同期＋ポーリング方式で扱う:

- ワーカは実行中、約300ms間隔で **進捗 `{scanned, matched}`** をサーバへ報告する。
  コントロールページと GUI の左ペインに「running … scanned N, matched M」が刻々と出るので
  「固まっていない」ことが分かる（受け取り→即応答ではなく、逐次経過が見える）。
- 実行はジョブごとに goroutine で回すため、長い find の最中もワーカは他ジョブを受けられる。
- コントロールページの running 行に出る **「中断」ボタン**でキャンセル要求 → ワーカは次の
  進捗報告の応答でそれを検知し、`ctx` を止めて中断（`canceled`、途中までの結果を保持）。

## 共通フラグ / 環境変数
| フラグ | 環境変数 | 既定 |
| --- | --- | --- |
| `--endpoint` | `CSRPC_ENDPOINT` | `http://127.0.0.1:8080/rpc` |
| `--timeout` | `CSRPC_TIMEOUT` | `30s` |
| `--retries` | `CSRPC_RETRIES` | `2` |
| `--header k=v`（複数可） | — | なし |
| `--output json\|raw` | — | `json` |
| `--log-level debug\|info` | `CSRPC_LOG` | `info` |

- `--param k=v` は文字列値の簡易指定。複雑な型は `--params-json` を使う。
- `--idempotent` を付けた呼び出しのみ、トランスポートエラー時に再送する（安全側の既定は再送なし）。
- `--header` は将来の認証トークン注入（`Authorization=Bearer ...` 等）に流用できる。

## 終了コード
| コード | 意味 |
| --- | --- |
| 0 | 成功 |
| 1 | 業務エラー（サーバが `error` を返した / RPCError） |
| 2 | トランスポート・使用法エラー（接続失敗・タイムアウト・引数不正） |

## ライブラリとして使う
```go
import "csrpc/internal/rpc"

c, _ := rpc.New(rpc.Options{Endpoint: "http://127.0.0.1:8080/rpc"})
var out map[string]any
err := c.Call(ctx, "echo", map[string]string{"message": "hi"}, &out)
// err は *rpc.RPCError（業務エラー）か *rpc.TransportError（通信失敗）で判別可能
```

## テスト
```bash
go test ./...      # httptest によるモックサーバで正常/エラー/リトライ/タイムアウトを検証
```
