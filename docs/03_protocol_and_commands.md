# プロトコル & コマンド仕様 — cs-rpc

サーバ・クライアント間でやりとりする**全エンドポイント**と、**コマンド（メソッド）と
パラメータ**の一覧リファレンス。実装の現状（コントロールプレーン／ワーカ実行／進捗・
キャンセル／find）を反映する。

- 関連: [全体設計](00_overview.md) / [クライアント設計](01_client_go.md) / [サーバ設計](02_server_python.md)

---

## 1. 2つの平面（データプレーン / コントロールプレーン）

cs-rpc の通信は目的の異なる2系統に分かれる。

| 平面 | パス | 形式 | 誰が使うか | 実行場所 |
| --- | --- | --- | --- | --- |
| **データプレーン** | `POST /rpc` | JSON-RPC 2.0 | CLI/ライブラリ（`csrpc call` 等） | **サーバ**が即時実行 |
| **コントロールプレーン** | `POST /control/*` | REST(JSON) | コントロールページ（操作者）＋ワーカ | キュー経由で**ワーカ**（またはサーバ自動実行） |

- **データプレーン**: 1リクエスト=1レスポンスの同期 RPC。`csrpc call echo ...` はこれ。
  仕様は [00_overview.md 3章](00_overview.md) を参照。
- **コントロールプレーン**: 「次に実行するコマンド」をキューに積み、**別ホストのワーカ**が
  取りに来て実行する非同期モデル。デモの中心。以下で詳述する。

文字コードは全経路 **UTF-8 固定**。Content-Type は `application/json`。

---

## 2. コマンド（メソッド）カタログ

`method` + `params`（JSON オブジェクト）→ `result`（JSON） が基本形。

### 2.1 一覧

| method | params | result | サーバ | ワーカ | 冪等 | 備考 |
| --- | --- | --- | :-: | :-: | :-: | --- |
| `echo` | `{message: string}` | `{message: string}` | ✓ | ✓ | ✓ | 受け取った文字列をそのまま返す |
| `math.add` | `{a: number, b: number}` | `{result: number}` | ✓ | ✓ | ✓ | 加算 |
| `math.div` | `{a: number, b: number}` | `{result: number}` | ✓ | ✓ | ✓ | `b=0` はドメインエラー **1001** |
| `sys.time` | なし | `{epoch, iso}` | ✓ | ✓ | ✓ | **実行したホスト**の時刻 |
| `sys.info` | なし | 下記参照 | ✓ | ✓ | ✓ | **実行した側**の情報。サーバ実行とワーカ実行で内容が異なる |
| `demo.sleep` | `{seconds: number}` | `{slept: number}` | ― | ✓ | ― | 指定秒スリープ（最大10s）。running 可視化用。`ctx` で中断可 |
| `find` | `{path, name, maxResults}` | 下記参照 | ― | ✓ | ✓ | パス配下を走査。**長時間・進捗・キャンセル対応**（4章） |
| `exec` | `{program, args?, wait?}` | 下記参照 | ― | ✓ | ― | **外部プログラム実行**。allowlist 必須・既定無効（2.4・⚠️セキュリティ） |

- 「サーバ」= サーバ側 Python ハンドラに登録済み（`run-now`/`step`/autorun で実行される）。
- 「ワーカ」= Go クライアントの `worker` がローカル実行できる（enqueue → lease で実行）。
- `find` / `demo.sleep` は**ワーカ専用**。enqueue してワーカに実行させる経路でのみ動く。

### 2.2 実行場所によって結果が変わる例: `sys.info`
同じ `sys.info` でも、どちらで実行されたかで返る内容が異なる（＝分散実行の可視化に使える）。

サーバ実行（`run-now` 等）:
```json
{ "python": "3.12.3", "system": "Linux", "release": "6.x", "machine": "aarch64" }
```
ワーカ実行（Windows クライアント等）:
```json
{ "executedOn": "client", "os": "windows", "arch": "amd64",
  "host": "PC07", "goVersion": "go1.22.5" }
```

### 2.3 `find` の入出力
リクエスト params:
```json
{ "path": "/etc", "name": "*.conf", "maxResults": 1000 }
```
| フィールド | 型 | 既定 | 意味 |
| --- | --- | --- | --- |
| `path` | string | `"."` | 走査を開始するディレクトリ |
| `name` | string(glob) | `"*"` | ファイル名（ベース名）のグロブ一致条件 |
| `maxResults` | int | `1000` | 収集件数の上限（超えたら打ち切り） |

result:
```json
{ "matches": ["/etc/hosts.conf", "..."], "scanned": 12873,
  "matched": 128, "truncated": false }
```
- `scanned`: 走査したエントリ数、`matched`: 一致数、`truncated`: 上限で打ち切ったか。
- 読めないエントリ（権限エラー等）はスキップして続行する。

### 2.4 `exec`（外部プログラム実行）
クライアント上で外部プログラム/OSコマンドを実行する。`calc.exe` の起動や、`dir` の
出力回収などに使う。

リクエスト params:
```json
{ "program": "calc.exe", "args": [], "wait": false }
```
| フィールド | 型 | 既定 | 意味 |
| --- | --- | --- | --- |
| `program` | string | （必須） | 実行するプログラム名/パス |
| `args` | string[] | `[]` | 引数 |
| `wait` | bool | `false` | `false`=起動して即完了（突き放し）/ `true`=完了まで待ち出力を回収 |

result（`wait=false`／起動のみ）:
```json
{ "started": true, "pid": 12345, "program": "calc.exe" }
```
result（`wait=true`／出力回収）:
```json
{ "exitCode": 0, "stdout": "…", "stderr": "" }
```
- `wait=true` は `ctx` に紐づくため、コントロールページの「中断」でプロセスを止められる。
- 出力は最大 64KB で打ち切り（`…(truncated)`）。

> ### ⚠️ セキュリティ（exec）
> `exec` は実質**リモートコード実行**である。無制限だと、サーバの enqueue を叩ける者が
> 各クライアントで任意コマンドを実行できてしまう。そのため:
> - **既定は無効**。実行側（ワーカ）の環境変数 **`CSRPC_EXEC_ALLOW`**（カンマ区切り）に
>   許可プログラム名を列挙したときだけ、その中のものだけ実行できる。
>   例: `CSRPC_EXEC_ALLOW=calc,notepad`（比較はベース名・小文字・`.exe` 除去で正規化）。
> - 未設定なら `error 1003`（disabled）、allowlist 外なら `error 1002`（not allowed）。
> - 制限は**実行するクライアント側で強制**される（サーバ/操作者は上書きできない）。
> - 信頼できるネットワーク限定で使うこと。外部公開の可能性があるなら認証を先に入れる。

### 2.5 エラー
`result` の代わりに `error`（JSON-RPC 準拠）で返す。コード体系は
[00_overview.md 3.4](00_overview.md) と一致。

| code | 意味 |
| --- | --- |
| `-32601` | メソッド未登録（`no local handler` 含む） |
| `-32602` | パラメータ不正 |
| `-32603` | 内部エラー |
| `1001` | （`math.div`）ゼロ除算 |
| `1002` | （`exec`）allowlist 外のプログラム |
| `1003` | （`exec`）allowlist 未設定で無効 |
| `1004` | （`exec`）起動/実行失敗 |

---

## 3. コントロールプレーン API

ジョブは次の状態を遷移する:

```
enqueue        lease            complete / cancel
   │             │                     │
   ▼             ▼                     ▼
 queued ──────▶ running ──────▶ done | error | canceled
   └── cancel(queued) ─────────────▶ canceled
```

ジョブオブジェクト（`state` 等に現れる形）:
```json
{
  "id": "ab12cd34", "seq": 7, "method": "find",
  "params": { "path": "/etc", "name": "*.conf" },
  "state": "running",              // queued|running|done|error|canceled
  "source": "web", "worker": "PC07",
  "progress": { "scanned": 8000, "matched": 12 },   // 実行中のみ
  "result": null, "error": null,
  "created_at": 1700000000.0, "updated_at": 1700000001.2
}
```

### 3.1 操作者（コントロールページ）向け

| メソッド | パス | ボディ | 応答 | 用途 |
| --- | --- | --- | --- | --- |
| GET | `/` | ― | HTML | コントロールページ |
| GET | `/control/state` | ― | スナップショット※ | キュー/実行中/履歴/autorun を取得（ページが約1秒間隔でポーリング） |
| POST | `/control/enqueue` | `{method, params?, source?}` | ジョブ | 「次に実行するコマンド」を積む。`method` は既知（サーバ登録∪ワーカ申告）でないと 400 |
| POST | `/control/run-now` | `{method, params?}` | `{ok, result}` / `{ok:false, error}` | **サーバ上で即時実行**（サーバ登録メソッドのみ） |
| POST | `/control/step` | ― | ジョブ / 204 | queued を1件**サーバ上で**実行 |
| POST | `/control/autorun` | `{enabled: bool}` | `{autorun}` | サーバ自動実行の ON/OFF |
| POST | `/control/cancel` | `{id}` | `{id, result}` | queued は除去(`canceled`)、running は中断要求(`cancel_requested`) |
| POST | `/control/clear` | ― | `{cleared}` | 履歴クリア |

※スナップショット:
```json
{ "autorun": false, "tick": 0.7,
  "methods": ["echo","find","math.add", "..."],   // サーバ登録∪ワーカ申告
  "pending": [ /* queued ジョブ */ ],
  "running": [ /* running ジョブ */ ],
  "history": [ /* done/error/canceled（新しい順） */ ] }
```

### 3.2 ワーカ（Go クライアント `worker`）向け

| メソッド | パス | ボディ | 応答 | 用途 |
| --- | --- | --- | --- | --- |
| POST | `/control/announce` | `{worker, methods:[...]}` | `{known:[...]}` | 起動時、自分が実行できるメソッドを申告（選択肢に反映） |
| POST | `/control/lease` | `{worker}` | ジョブ / **204** | 次の queued を取得（running にする）。無ければ 204 |
| POST | `/control/progress` | `{id, progress}` | `{cancel: bool}` | 途中経過を報告。**応答の `cancel` が中断要求** |
| POST | `/control/complete` | `{id, result?, error?, canceled?}` | ジョブ | 完了報告。`canceled:true` で中断完了 |

---

## 4. 長時間コマンドの扱い（find の進捗・キャンセル）

`find` のような長時間処理を「受け取ったら黙って処理し、終わってから答える」だけにすると
**応答不能に見える**。そこで非同期＋ポーリングで進捗を可視化し、中断もできるようにする。

### 4.1 シーケンス
```
操作者(ページ)         サーバ(キュー)              ワーカ(別ホスト)
   │  enqueue find        │                          │
   │─────────────────────▶│  queued                  │
   │                      │◀───── lease {worker} ────│   （running へ）
   │                      │──────── job ────────────▶│   find 開始
   │  poll /control/state │◀─ progress{scanned,..} ──│   ~300ms 毎に報告
   │  （running+progress）│──── {cancel:false} ─────▶│
   │  ［中断］ボタン       │                          │
   │──── cancel {id} ────▶│  cancel_requested        │
   │                      │◀─ progress ──────────────│
   │                      │──── {cancel:true} ──────▶│   ctx 停止 → 中断
   │                      │◀─ complete{canceled} ────│   途中結果を保持
   │  （canceled 表示）    │                          │
```

### 4.2 ルール
- ワーカは実行中、約 **300ms 間隔**で `progress`（`find` は `{scanned, matched}`）を送る。
  コントロールページと GUI は running のジョブにこれを逐次表示する。
- **キャンセル**: 操作者が `/control/cancel` を呼ぶ → サーバがそのジョブに `cancel_requested`
  を立てる → ワーカは次の `progress` の応答 `{cancel:true}` で気づき、実行コンテキストを
  止めて中断する。結果は `canceled` となり、**途中までの結果**（`find` なら `scanned`/
  一部 `matches`）を保持する。
- ワーカはジョブごとに並行実行するため、長い `find` の最中も別コマンドを受け付けられる。

---

## 5. データプレーン（`/rpc`）との関係

`POST /rpc`（JSON-RPC）は**サーバ上で即時実行**する同期経路で、`csrpc call` / `ping` /
`methods` が使う。コントロールプレーンの `run-now` はこれと同様にサーバ実行だが、
コントロールプレーンの本命は **enqueue → ワーカ実行**（別ホストのクライアントが処理）で
ある。両者は同じ「method + params → result/error」の意味論を共有する。

- `csrpc call <method> --params-json '{...}'` … データプレーン（サーバ実行）
- コントロールページで method+params を enqueue … コントロールプレーン（ワーカ実行）

詳細な JSON-RPC の封筒・エラーコード・HTTP ステータス対応は
[00_overview.md 3章](00_overview.md) を参照。
