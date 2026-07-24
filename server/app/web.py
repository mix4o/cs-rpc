"""コントロールページと制御 API（設計: デモ用の Web コントロール機能）。

- GET  /               コントロールページ（HTML）
- GET  /control/state  キュー・実行中・履歴・autorun 状態の JSON（ページがポーリング）
- POST /control/enqueue  次に実行するコマンドを挿入（1台が取る共有ジョブ）
- POST /control/broadcast 接続中の全クライアントへ同じ命令を配信（クライアント数だけ複製）
- POST /control/run-now  キューを介さず即時実行して結果を返す
- POST /control/step     queued を1件サーバ実行
- POST /control/autorun  サーバ自動実行の ON/OFF
- POST /control/cancel   queued ジョブのキャンセル
- POST /control/clear    履歴クリア
- GET  /control/clients  接続中クライアント（ワーカ）一覧
- 外部ワーカ用:
  - POST /control/announce  接続申告（クライアント登録 + 実行可能メソッド申告）
  - POST /control/lease     次の queued を lease（無ければ 204。ポーリングが heartbeat）
  - POST /control/progress  途中経過報告（heartbeat 兼）
  - POST /control/complete  lease したジョブの結果を報告
"""
from __future__ import annotations

import asyncio
import logging
import time
from pathlib import Path
from typing import Any

from fastapi import APIRouter, HTTPException
from fastapi.responses import HTMLResponse, JSONResponse, Response
from pydantic import BaseModel

from app.config import settings
from app.control import JobState, store
from app.dispatcher import dispatch, registry
from app.presets import PresetError, presets
from app.protocol import RpcException

log = logging.getLogger("csrpc.web")
router = APIRouter()

_INDEX = Path(__file__).parent / "static" / "index.html"

# 実行中のプリセット逐次実行タスク（GC 防止に参照を保持）
_preset_tasks: set[asyncio.Task] = set()

_TERMINAL = (JobState.DONE, JobState.ERROR, JobState.CANCELED)


async def _await_terminal(job_id: str, timeout: float) -> None:
    """ジョブが終端状態になるまで待つ（タイムアウトで諦めて次へ）。"""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        job = store.get_job(job_id)
        if job is not None and job.state in _TERMINAL:
            return
        await asyncio.sleep(0.1)


async def _run_preset_sequence(name: str, steps: list[dict]) -> None:
    """プリセットを逐次実行する。command は投入→完了待ち、{wait} はサーバ側で待機。"""
    for step in steps:
        if "wait" in step:
            await asyncio.sleep(min(float(step["wait"]), 3600.0))
            continue
        job = store.enqueue(step["method"], step.get("params"), source=f"preset:{name}")
        await _await_terminal(job.id, settings.handler_timeout)
    log.info("preset %s sequence finished", name)


class EnqueueBody(BaseModel):
    method: str
    params: dict | list | None = None
    source: str = "web"


class BroadcastBody(BaseModel):
    method: str
    params: dict | list | None = None
    source: str = "broadcast"
    online_only: bool = True   # False にすると offline のクライアント宛にも積む


class AutorunBody(BaseModel):
    enabled: bool


class LeaseBody(BaseModel):
    worker: str = "worker"


class CompleteBody(BaseModel):
    id: str
    result: Any | None = None
    error: dict | None = None
    canceled: bool = False


class IdBody(BaseModel):
    id: str


class ProgressBody(BaseModel):
    id: str
    progress: dict


class AnnounceBody(BaseModel):
    worker: str = "worker"
    methods: list[str] = []


class PresetBody(BaseModel):
    name: str
    description: str = ""
    # command ステップ {method, params} と制御ステップ {wait} を混在できるよう生 dict で受け、
    # 実際の検証は presets._validate に委ねる。
    commands: list[dict[str, Any]]


class NameBody(BaseModel):
    name: str


@router.get("/", response_class=HTMLResponse)
async def index() -> HTMLResponse:
    return HTMLResponse(_INDEX.read_text(encoding="utf-8"))


@router.get("/control/state")
async def state() -> JSONResponse:
    return JSONResponse(store.snapshot())


@router.post("/control/enqueue")
async def enqueue(body: EnqueueBody) -> dict:
    # サーバ登録メソッドに加え、ワーカが申告したメソッド（例: find）も許可する。
    if body.method not in store.known_methods():
        raise HTTPException(status_code=400, detail=f"unknown method: {body.method}")
    job = store.enqueue(body.method, body.params, source=body.source)
    return job.to_dict()


@router.post("/control/broadcast")
async def broadcast(body: BroadcastBody) -> dict:
    """接続中の全クライアントへ同じ命令を配信（各クライアント宛に1つずつ複製投入）。

    呼び出し時点のクライアント一覧に対して積んで即座に返す（ファイア・アンド・
    フォーゲット）。誰が受け取るか・受け取ったかに関係なく、続けて次の命令を
    送れる。0台でも 200 を返す（targets:0）。
    """
    if body.method not in store.known_methods():
        raise HTTPException(status_code=400, detail=f"unknown method: {body.method}")
    jobs = store.broadcast(body.method, body.params, source=body.source,
                           online_only=body.online_only)
    group = jobs[0].group if jobs else None
    return {
        "broadcast": True,
        "group": group,
        "targets": len(jobs),
        "workers": [j.target for j in jobs],
        "ids": [j.id for j in jobs],
    }


@router.post("/control/run-now")
async def run_now(body: EnqueueBody) -> dict:
    """キューを介さず即時実行（クイックデモ用）。"""
    if body.method not in registry():
        raise HTTPException(status_code=400, detail=f"unknown method: {body.method}")
    try:
        result = await dispatch(body.method, body.params)
        return {"ok": True, "result": result}
    except RpcException as e:
        return {"ok": False, "error": {"code": e.code, "message": e.message, "data": e.data}}


@router.post("/control/step")
async def step() -> Response:
    job = await store.step()
    if job is None:
        return Response(status_code=204)
    return JSONResponse(job.to_dict())


@router.post("/control/autorun")
async def autorun(body: AutorunBody) -> dict:
    store.set_autorun(body.enabled)
    return {"autorun": store.autorun}


@router.post("/control/cancel")
async def cancel(body: IdBody) -> dict:
    result = store.cancel(body.id)
    if result == "not_found":
        raise HTTPException(status_code=404, detail="job not found or not cancelable")
    # canceled = queued を除去 / cancel_requested = running へ中断要求
    return {"id": body.id, "result": result}


@router.post("/control/clear")
async def clear() -> dict:
    store.clear_history()
    return {"cleared": True}


# --- 外部ワーカ用 ---
@router.post("/control/lease")
async def lease(body: LeaseBody) -> Response:
    job = store.lease(body.worker)
    if job is None:
        return Response(status_code=204)
    return JSONResponse(job.to_dict())


@router.post("/control/complete")
async def complete(body: CompleteBody) -> dict:
    job = store.complete(body.id, result=body.result, error=body.error, canceled=body.canceled)
    if job is None:
        raise HTTPException(status_code=404, detail="running job not found")
    return job.to_dict()


@router.post("/control/progress")
async def progress(body: ProgressBody) -> dict:
    """ワーカからの途中経過報告。応答で中断要求(cancel)の有無を返す。"""
    cancel_requested = store.report_progress(body.id, body.progress)
    return {"cancel": cancel_requested}


@router.post("/control/announce")
async def announce(body: AnnounceBody) -> dict:
    """ワーカが接続を申告（クライアント登録 + 実行可能メソッドを選択肢に反映）。"""
    store.announce(body.worker, body.methods)
    return {"known": sorted(store.known_methods()), "clients": store.clients()}


@router.get("/control/clients")
async def clients() -> dict:
    """接続中クライアント（ワーカ）一覧。online は heartbeat の鮮度から算出。"""
    return {"clients": store.clients(), "timeout": settings.client_timeout}


# --- プリセット（複数コマンドをまとめて登録・一括投入） ---
@router.get("/control/presets")
async def list_presets() -> dict:
    return {"presets": presets.list()}


@router.post("/control/presets")
async def save_preset(body: PresetBody) -> dict:
    try:
        return presets.put(body.model_dump())
    except PresetError as e:
        raise HTTPException(status_code=400, detail=str(e))


@router.post("/control/presets/reload")
async def reload_presets() -> dict:
    """ディレクトリを再スキャン（ファイルを手で置き換えた後、再起動せず反映）。"""
    return {"count": presets.reload()}


@router.post("/control/presets/delete")
async def delete_preset(body: NameBody) -> dict:
    if not presets.delete(body.name):
        raise HTTPException(status_code=404, detail="preset not found")
    return {"deleted": body.name}


@router.post("/control/presets/run")
async def run_preset(body: NameBody) -> dict:
    """プリセットを逐次実行する（command は投入、{wait} はサーバ側で待機）。

    待機を含むため、実行はバックグラウンドで進め、HTTP はすぐ返す。
    """
    preset = presets.get(body.name)
    if preset is None:
        raise HTTPException(status_code=404, detail="preset not found")
    steps = preset["commands"]
    task = asyncio.create_task(_run_preset_sequence(body.name, steps))
    _preset_tasks.add(task)
    task.add_done_callback(_preset_tasks.discard)
    return {
        "preset": body.name,
        "started": True,
        "commands": sum(1 for s in steps if "method" in s),
        "waits": sum(1 for s in steps if "wait" in s),
    }
