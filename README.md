# cs-rpc

Windows / Linux で動作する、クライアント・サーバ間の汎用リクエスト応答（RPC）基盤。

- **クライアント**: Go（単一バイナリ、Windows/Linux 配布）
- **サーバ**: Python / FastAPI（コマンド追加が容易）
- **通信**: HTTP + JSON（JSON-RPC 2.0 準拠）
- **認証**: 初期版は無し（ローカル/信頼環境）。後付けできる設計。

## 設計ドキュメント
| 文書 | 内容 |
| --- | --- |
| [docs/00_overview.md](docs/00_overview.md) | 全体設計・基盤 RPC プロトコル・シーケンス |
| [docs/01_client_go.md](docs/01_client_go.md) | Go クライアント設計 |
| [docs/02_server_python.md](docs/02_server_python.md) | Python サーバ設計 |
| [docs/03_protocol_and_commands.md](docs/03_protocol_and_commands.md) | **全エンドポイント & コマンド/パラメータ仕様**（コントロールプレーン・find・進捗/キャンセル） |

## 全体像
```
Client (Go)  ──HTTP POST /rpc (JSON-RPC 2.0)──▶  Server (Python / FastAPI)
             ◀────────── JSON result/error ─────  Dispatcher + Handlers
```

## 実装状況
| 部位 | 状態 | 場所 |
| --- | --- | --- |
| サーバ雛形 (Python/FastAPI) | ✅ 実装・テスト済み | [server/](server/) |
| クライアント雛形 (Go) | ✅ 実装・テスト済み | [client/](client/) |

### クイックスタート
```bash
# 1) サーバ起動
cd server && pip install -e ".[dev]" && uvicorn app.main:app --port 8080

# 2) 別端末でクライアント
cd client && go build -o csrpc ./cmd/csrpc
CSRPC_ENDPOINT=http://127.0.0.1:8080/rpc ./csrpc call echo --param message=hello
```

同梱サンプルメソッド: `echo` / `math.add` / `math.div` / `sys.time` / `sys.info`。
新しいコマンドはサーバの `server/app/handlers/` に関数を1つ足すだけで追加できます。

### デモの流れ（Web コントロール + GUI ワーカ）
```bash
# 1) サーバ起動 → ブラウザで http://localhost:8080/ （コントロールページ）
cd server && uvicorn app.main:app --port 8080

# 2) 別端末で GUI 付きワーカ起動 → http://127.0.0.1:8787 が開く
cd client && go build -o csrpc ./cmd/csrpc
CSRPC_ENDPOINT=http://127.0.0.1:8080/rpc ./csrpc worker
```
コントロールページから「次に実行するコマンド」を挿入すると、ワーカがそれを
**クライアント側で実行**し、GUI の左ペイン（コマンドと結果）・右ペイン（処理ログ）に
リアルタイム表示されます。`sys.info` はワーカ機の情報を返すので、サーバ指示→クライアント
実行の分散動作が一目で分かります。

> 研修で1台のPC上にサーバとクライアントを同居させる場合、クライアントを
> `go build -tags webview` でビルドすると GUI が**独立したネイティブウィンドウ**
> （タイトル `cs-rpc CLIENT — worker: <名前>`・CLIENT バッジ・teal 配色）で開き、
> ブラウザで開くサーバのコントロールページと一目で区別できます。
> 詳細・OS別の依存/ビルド手順は [client/README.md](client/README.md#ネイティブウィンドウ表示-tags-webview)。

- サーバ側 Web コントロール: [server/README.md](server/README.md#コントロールページデモ用-web-ui)
- クライアント GUI ワーカ: [client/README.md](client/README.md#worker-サブコマンドgui-付きデモワーカ)
