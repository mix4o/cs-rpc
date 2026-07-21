# サーバ設計書 — cs-rpc (Python)

RPC リクエストを受け、登録されたコマンド（ハンドラ）へディスパッチして
結果を返すサーバ。「変更しやすさ」＝ハンドラ追加の容易さを最優先する。

- 関連: [全体設計書](00_overview.md) / [クライアント設計書](01_client_go.md) / [プロトコル&コマンド仕様](03_protocol_and_commands.md)

> 本書は `/rpc`（同期 RPC）サーバの設計。コントロールページ・ジョブキュー・
> `/control/*` API（enqueue/lease/progress/cancel/announce）の仕様は
> [03_protocol_and_commands.md](03_protocol_and_commands.md) を参照。

---

## 1. 役割と設計方針

### 1.1 役割
- `POST /rpc` で JSON-RPC リクエストを受信・検証。
- `method` に対応するハンドラへディスパッチし、`params` を渡して実行。
- 結果を `result`、例外を `error` に変換して JSON-RPC 応答を返す。
- `/healthz`（死活）・`/rpc/methods`（自己記述）を提供。

### 1.2 方針
- **フレームワーク: FastAPI + Uvicorn**。
  型ヒントによる自動バリデーション（Pydantic）、少ないコードで堅牢、
  自動 API ドキュメント（`/docs`）が付くため開発・デバッグが速い。
- **ハンドラ登録は宣言的**: デコレータ `@register("method.name")` を付けるだけで
  ディスパッチ表に載る。**新コマンド追加＝ファイル1つ / 関数1つ**を実現する。
- **薄い層構造**: プロトコル処理（protocol）／振り分け（dispatcher）／
  業務ロジック（handlers）を分離。業務を足すときは handlers だけ触る。

> 代替案: 依存を一切増やしたくない場合は標準ライブラリ `http.server` でも実装可能。
> ただしバリデーション・ルーティング・並行性を自作する必要があり、
> 「変更しやすさ」を損なうため初期採用は FastAPI とする。

---

## 2. コンポーネント構成

```
server/
├── pyproject.toml
├── app/
│   ├── main.py            # FastAPI アプリ生成・エンドポイント定義
│   ├── protocol.py        # JSON-RPC の Pydantic モデル・エラー定義
│   ├── dispatcher.py      # register デコレータ + メソッドレジストリ
│   ├── middleware.py      # ロギング・相関ID・（将来）認証フック
│   ├── config.py          # 設定（環境変数 > .env > 既定）
│   └── handlers/
│       ├── __init__.py    # ハンドラ自動 import（登録の副作用を起こす）
│       ├── echo.py        # 例: echo
│       └── job.py         # 例: job.run など
└── tests/
    ├── test_dispatcher.py
    ├── test_handlers.py
    └── test_protocol.py
```

---

## 3. プロトコル処理（protocol.py）

Pydantic モデルで JSON-RPC を表現し、受信時に自動検証する。

```python
class RpcRequest(BaseModel):
    jsonrpc: Literal["2.0"]
    id: str | int | None = None
    method: str
    params: dict | list | None = None

class RpcError(BaseModel):
    code: int
    message: str
    data: Any | None = None

class RpcResponse(BaseModel):
    jsonrpc: Literal["2.0"] = "2.0"
    id: str | int | None
    result: Any | None = None
    error: RpcError | None = None
```

- 検証失敗（型/必須不足）は `-32600 Invalid Request` / `-32602 Invalid params`。
- JSON パース失敗は `-32700 Parse error`。
- アプリ固有例外は `RpcException(code, message, data)` を投げ、
  ディスパッチャが `error` に変換する。

### 3.1 エラーコード（全体設計 3.4 と一致）
| code | 定数 | 用途 |
| --- | --- | --- |
| -32700 | `PARSE_ERROR` | JSON 破損 |
| -32600 | `INVALID_REQUEST` | 形式不正 |
| -32601 | `METHOD_NOT_FOUND` | 未登録メソッド |
| -32602 | `INVALID_PARAMS` | パラメータ不正 |
| -32603 | `INTERNAL_ERROR` | 未捕捉例外 |
| 1000〜 | （各ハンドラ定義） | ドメインエラー |

---

## 4. ディスパッチャとハンドラ登録（dispatcher.py）

```python
# レジストリ: method名 -> HandlerSpec
_REGISTRY: dict[str, HandlerSpec] = {}

def register(method: str, *, idempotent: bool = False,
             params_model: type[BaseModel] | None = None):
    def deco(fn):
        _REGISTRY[method] = HandlerSpec(fn, idempotent, params_model)
        return fn
    return deco

async def dispatch(method: str, params) -> Any:
    spec = _REGISTRY.get(method)
    if spec is None:
        raise RpcException(METHOD_NOT_FOUND, "Method not found",
                           data={"method": method})
    args = spec.validate(params)          # params_model があれば検証
    return await spec.call(args)          # 同期関数は thread pool で実行
```

### 4.1 ハンドラ実装例（handlers/echo.py）
```python
from app.dispatcher import register
from pydantic import BaseModel

class EchoParams(BaseModel):
    message: str

@register("echo", idempotent=True, params_model=EchoParams)
async def echo(p: EchoParams) -> dict:
    return {"message": p.message}
```

- **これがコマンド追加の全て**: 新ファイルを `handlers/` に置き `@register` を付ける。
  `handlers/__init__.py` が起動時に全モジュールを import して登録を確定させる
  （`pkgutil.iter_modules` で自動 import すると追加時に __init__ 編集も不要）。
- `idempotent=True` はクライアントのリトライ許可の指標として `/rpc/methods` で公開。
- 同期的で重い処理は `asyncio.to_thread` / thread pool に逃がし、
  イベントループをブロックしない。

---

## 5. エンドポイント（main.py）

```python
@app.post("/rpc")
async def rpc(request: Request):
    raw = await request.body()
    try:
        payload = json.loads(raw)
    except JSONDecodeError:
        return error_response(None, PARSE_ERROR, "Parse error")
    try:
        req = RpcRequest.model_validate(payload)
    except ValidationError as e:
        return error_response(payload.get("id"), INVALID_REQUEST, str(e))
    try:
        result = await dispatch(req.method, req.params)
        if req.id is None:                     # 通知: 応答なし
            return Response(status_code=204)
        return success_response(req.id, result)
    except RpcException as e:
        return error_response(req.id, e.code, e.message, e.data)
    except Exception as e:                     # 未捕捉
        log.exception("handler failed id=%s", req.id)
        return error_response(req.id, INTERNAL_ERROR, "Internal error")
```

| メソッド | パス | 実装 |
| --- | --- | --- |
| POST | `/rpc` | 上記ディスパッチ |
| GET | `/healthz` | `{"status":"ok"}` を返す軽量応答 |
| GET | `/rpc/methods` | レジストリから method 名・冪等性・paramsスキーマを列挙 |

- 全応答は HTTP 200（プロトコルエラーも `error` として 200 で返す）。
  ただし JSON パース不能ボディは 400 を付す（全体設計 3.5 に準拠）。
- リクエストボディ上限（既定 10MB）を超えたら 413 相当のエラー。

---

## 6. ミドルウェア / 横断処理（middleware.py）

| 関心事 | 実装 |
| --- | --- |
| 相関ID | リクエストの `id`（なければ採番）をログコンテキストに束縛 |
| 構造化ログ | `id / method / status / elapsed_ms` を JSON ログで出力 |
| タイムアウト | ハンドラ実行に上限（`asyncio.wait_for`、既定 60s） |
| 例外の握り | 未捕捉例外を `-32603` に集約（スタックはログのみ、外部に出さない） |
| **認証（将来）** | ここに差し込み口を用意。初期は素通し（no-op） |

### 6.1 認証フック（拡張の余地）
```python
async def authenticate(request) -> None:
    # 初期版: 何もしない。将来ここで Authorization ヘッダ / APIキーを検証。
    return
```
- 認証を有効化しても `/rpc` 本体のコードは変えず、ミドルウェア設定のみで切替可能に。

---

## 7. 設定（config.py）

優先順位: **環境変数 > `.env` > 既定値**。

| 項目 | 環境変数 | 既定 |
| --- | --- | --- |
| バインドアドレス | `CSRPC_HOST` | `127.0.0.1` |
| ポート | `CSRPC_PORT` | `8080` |
| ハンドラ実行上限 | `CSRPC_HANDLER_TIMEOUT` | `60`（秒） |
| ボディ上限 | `CSRPC_MAX_BODY` | `10485760`（10MB） |
| ログレベル | `CSRPC_LOG_LEVEL` | `INFO` |
| ワーカ数 | `CSRPC_WORKERS` | `1` |

- 既定は `127.0.0.1` バインド（ローカル/信頼環境前提）。外部公開時は
  リバースプロキシ（TLS 終端）越しにする想定。

---

## 8. 起動と運用

```
# 開発
uvicorn app.main:app --reload --host 127.0.0.1 --port 8080

# 本番相当（Linux）
uvicorn app.main:app --host 0.0.0.0 --port 8080 --workers 4
```

- Windows でも同じ Uvicorn コマンドで起動可能（クロスプラットフォーム）。
- コンテナ化する場合はスリムな Python イメージ＋非rootユーザで実行。
- ヘルスチェックは `/healthz`（オーケストレータの liveness/readiness に使用）。

---

## 9. テスト方針
- `test_protocol.py`: 全体設計 3章の JSON 例で round-trip・エラーコード検証。
- `test_dispatcher.py`: 登録/未登録/パラメータ不正/例外→errorマッピング。
- `test_handlers.py`: 各ハンドラ単体テスト（純粋関数として検証しやすい構造）。
- FastAPI `TestClient` による `/rpc` の結合テスト（正常/各種エラー）。

---

## 10. 拡張ポイント
- **新コマンド**: `handlers/` に関数を足して `@register` するだけ（4章）。
- **認証**: ミドルウェアの `authenticate` を実装（6.1）。本体コード不変。
- **TLS**: 直付けなら Uvicorn の `--ssl-keyfile/--ssl-certfile`、
  実運用はリバースプロキシで終端を推奨。
- **非同期ジョブ**: 長時間処理は「受付 → ジョブID返却 → `job.status` でポーリング」の
  メソッド群として追加できる（同期モデルを壊さず拡張）。
- **永続化・外部連携**: ハンドラ内から DB / 外部 API を呼ぶ。
  依存注入は FastAPI の `Depends` を利用してテスト容易性を保つ。
