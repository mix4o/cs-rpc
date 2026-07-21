"""FastAPI アプリ本体。エンドポイント定義（設計書 02 の5章）。"""
from __future__ import annotations

import asyncio
import json
import logging
from contextlib import asynccontextmanager

from fastapi import FastAPI, Request, Response

from app import handlers
from app.config import settings
from app.control import store
from app.dispatcher import dispatch, registry
from app.middleware import AccessLogMiddleware, authenticate
from app.web import router as web_router
from app.protocol import (
    INTERNAL_ERROR,
    INVALID_REQUEST,
    PARSE_ERROR,
    RpcException,
    RpcRequest,
    failure,
    success,
)

log = logging.getLogger("csrpc")


@asynccontextmanager
async def lifespan(app: FastAPI):
    logging.basicConfig(
        level=settings.log_level,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    loaded = handlers.load_all()
    log.info("handlers loaded: %s", ", ".join(loaded) or "(none)")
    await store.start()  # autorun ループ開始
    log.info("control queue started (autorun=%s)", store.autorun)
    try:
        yield
    finally:
        await store.stop()


app = FastAPI(title="cs-rpc server", version="0.1.0", lifespan=lifespan)
app.add_middleware(AccessLogMiddleware)
app.include_router(web_router)


@app.get("/healthz")
async def healthz() -> dict:
    return {"status": "ok"}


@app.get("/rpc/methods")
async def methods() -> dict:
    items = []
    for name, spec in sorted(registry().items()):
        schema = spec.params_model.model_json_schema() if spec.params_model else None
        items.append({"method": name, "idempotent": spec.idempotent, "params": schema})
    return {"methods": items}


@app.post("/rpc")
async def rpc(request: Request) -> Response:
    await authenticate(request)

    raw = await request.body()
    if len(raw) > settings.max_body:
        return _json(failure(None, INVALID_REQUEST, "Request body too large"),
                     status=413)

    # 1) JSON パース
    try:
        payload = json.loads(raw) if raw else None
    except json.JSONDecodeError:
        return _json(failure(None, PARSE_ERROR, "Parse error"), status=400)

    # 2) JSON-RPC 形式の検証
    try:
        req = RpcRequest.model_validate(payload)
    except Exception as e:  # pydantic ValidationError 含む
        rpc_id = payload.get("id") if isinstance(payload, dict) else None
        return _json(failure(rpc_id, INVALID_REQUEST, "Invalid Request",
                             data=str(e)))

    # 3) ディスパッチ（ハンドラ実行に上限を設ける）
    try:
        result = await asyncio.wait_for(
            dispatch(req.method, req.params), timeout=settings.handler_timeout
        )
    except RpcException as e:
        return _json(failure(req.id, e.code, e.message, e.data))
    except asyncio.TimeoutError:
        return _json(failure(req.id, INTERNAL_ERROR, "Handler timed out"))
    except Exception:
        log.exception("handler failed id=%s method=%s", req.id, req.method)
        return _json(failure(req.id, INTERNAL_ERROR, "Internal error"))

    # 通知（id なし）は本文なしで返す
    if req.id is None:
        return Response(status_code=204)
    return _json(success(req.id, result))


def _json(body: dict, status: int = 200) -> Response:
    return Response(
        content=json.dumps(body, ensure_ascii=False),
        media_type="application/json; charset=utf-8",
        status_code=status,
    )
