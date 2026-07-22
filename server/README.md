# cs-rpc server (Python / FastAPI)

汎用 JSON-RPC 2.0 サーバの雛形。設計は [../docs/02_server_python.md](../docs/02_server_python.md) を参照。

## セットアップ
```bash
cd server
python3 -m venv .venv
. .venv/bin/activate            # Windows: .venv\Scripts\activate
pip install -e ".[dev]"
```

## 起動
```bash
uvicorn app.main:app --reload --host 127.0.0.1 --port 8080
```
環境変数で上書き可: `CSRPC_HOST` / `CSRPC_PORT` / `CSRPC_HANDLER_TIMEOUT` /
`CSRPC_MAX_BODY` / `CSRPC_LOG_LEVEL`。

## 動作確認
```bash
curl -s localhost:8080/healthz
curl -s localhost:8080/rpc/methods

curl -s localhost:8080/rpc \
  -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":"1","method":"echo","params":{"message":"hi"}}'
```

## コントロールページ（デモ用 Web UI）
ブラウザで `http://localhost:8080/` を開くと、コマンドをキューに挿入して
実行・監視できるコントロールページが表示される。

- **method 選択 + params(JSON)** を入力し、
  - **キューに挿入 (enqueue)**: 「次に実行するコマンド」として保留キューに積む。
  - **即時実行 (run now)**: キューを介さずその場で実行して結果表示。
- **autorun（既定 ON）**: サーバがキューを自動で実行する。OFF にすると保留キューに
  溜まり、**step once** で1件ずつ、または外部ワーカ（下記）で処理できる。
- 保留キュー・実行履歴（result / error）を約1秒間隔で自動更新。
- 各履歴行の **「📋 copy」** で result/error をクリップボードにコピー、**「詳細」** で
  全文を下部ドロワー（テキストエリア）に表示して選択コピーできる（大きな出力・
  スクリプトの stdout 向け）。非HTTPS/LAN でも動くよう execCommand フォールバックあり。
- **プリセット**: 複数コマンドをまとめて名前付き登録し、「実行」で一括投入できる。
  JSON ファイル（`CSRPC_PRESETS_FILE`、既定 `presets.json`）に永続化され再起動後も残る。
  「＋現在のコマンドを追加」で入力中の method/params をプリセットに足せる。

### 制御 API
| メソッド | パス | 用途 |
| --- | --- | --- |
| GET | `/` | コントロールページ(HTML) |
| GET | `/control/state` | キュー・実行中・履歴・autorun 状態(JSON) |
| POST | `/control/enqueue` | `{method, params}` を挿入 |
| POST | `/control/run-now` | 即時実行して結果を返す |
| POST | `/control/step` | queued を1件サーバ実行 |
| POST | `/control/autorun` | `{enabled}` で自動実行 ON/OFF |
| POST | `/control/cancel` | `{id}`。queued は除去、running は中断要求を立てる |
| POST | `/control/clear` | 履歴クリア |
| POST | `/control/lease` | （外部ワーカ用）次の queued を lease、無ければ 204 |
| POST | `/control/complete` | （外部ワーカ用）`{id, result?, error?, canceled?}` を報告 |
| POST | `/control/progress` | （外部ワーカ用）`{id, progress}` を報告。応答 `{cancel}` で中断要求を返す |
| POST | `/control/announce` | （外部ワーカ用）`{worker, methods}` を申告し、選択肢に反映 |

長時間コマンド（例: ワーカの `find`）は、ワーカが実行中に `/control/progress` で
途中経過（`{scanned, matched}` 等）を定期報告し、コントロールページに逐次表示される。
running 行の「中断」ボタンは `/control/cancel` を呼び、ワーカは次の progress 応答で
中断要求を検知して停止する（`canceled`、途中結果を保持）。

### 外部ワーカで実行する（分散構成のデモ）
autorun を OFF にし、`/control/lease` → 実行 → `/control/complete` を回す
プロセスを別に立てると、Web で挿入したコマンドを別マシンのワーカが処理する構成を
デモできる。挙動確認用のワンライナー例:
```bash
curl -s localhost:8080/control/autorun -H 'content-type: application/json' -d '{"enabled":false}'
# enqueue 後...
JOB=$(curl -s localhost:8080/control/lease -H 'content-type: application/json' -d '{"worker":"demo"}')
echo "$JOB"   # {"id":...,"method":...,"params":...}
curl -s localhost:8080/control/complete -H 'content-type: application/json' \
  -d '{"id":"<上のid>","result":{"done":true}}'
```

### 設定（環境変数）
| 変数 | 既定 | 説明 |
| --- | --- | --- |
| `CSRPC_AUTORUN` | `true` | 起動時に自動実行を有効化 |
| `CSRPC_AUTORUN_TICK` | `0.7` | 自動実行ループ間隔(秒) |
| `CSRPC_HISTORY_LIMIT` | `100` | 履歴保持件数 |

## テスト
```bash
pytest
```

## コマンド（メソッド）の追加
`app/handlers/` に新しいファイルを置き、`@register` を付けるだけ。

```python
# app/handlers/greet.py
from pydantic import BaseModel
from app.dispatcher import register

class GreetParams(BaseModel):
    name: str

@register("greet", idempotent=True, params_model=GreetParams)
async def greet(p: GreetParams) -> dict:
    return {"text": f"hello, {p.name}"}
```
起動時に `handlers/__init__.py` が自動 import して登録します（__init__ の編集不要）。

## 同梱サンプルメソッド
| method | params | 説明 |
| --- | --- | --- |
| `echo` | `{message}` | そのまま返す |
| `math.add` | `{a,b}` | 加算 |
| `math.div` | `{a,b}` | 除算（b=0 でドメインエラー 1001） |
| `sys.time` | なし | サーバ時刻 |
| `sys.info` | なし | Python/OS 情報 |
