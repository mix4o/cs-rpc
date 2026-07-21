"""コントロールページと制御 API（設計: デモ用の Web コントロール機能）。

- GET  /               コントロールページ（HTML）
- GET  /control/state  キュー・実行中・履歴・autorun 状態の JSON（ページがポーリング）
- POST /control/enqueue  次に実行するコマンドを挿入
- POST /control/run-now  キューを介さず即時実行して結果を返す
- POST /control/step     queued を1件サーバ実行
- POST /control/autorun  サーバ自動実行の ON/OFF
- POST /control/cancel   queued ジョブのキャンセル
- POST /control/clear    履歴クリア
- 外部ワーカ用:
  - POST /control/lease     次の queued を lease（無ければ 204）
  - POST /control/complete  lease したジョブの結果を報告
"""
from __future__ import annotations

from pathlib import Path
from typing import Any

from fastapi import APIRouter, HTTPException
from fastapi.responses import HTMLResponse, JSONResponse, Response
from pydantic import BaseModel

from app.control import store
from app.dispatcher import dispatch, registry
from app.protocol import RpcException

router = APIRouter()

_INDEX = Path(__file__).parent / "static" / "index.html"


class EnqueueBody(BaseModel):
    method: str
    params: dict | list | None = None
    source: str = "web"


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
    """ワーカが実行可能メソッドを申告（コントロールページの選択肢に反映）。"""
    store.announce_methods(body.methods)
    return {"known": sorted(store.known_methods())}
