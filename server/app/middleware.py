"""横断処理: 相関ID・構造化ログ・認証フック（設計書 02 の6章）。"""
from __future__ import annotations

import logging
import time
import uuid

from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import Response

log = logging.getLogger("csrpc")


async def authenticate(request: Request) -> None:
    """認証フック。初期版は素通し（no-op）。

    将来ここで Authorization ヘッダ / API キーを検証する。有効化しても
    /rpc 本体のコードは変えず、この関数の実装だけで切り替えられる。
    """
    return None


class AccessLogMiddleware(BaseHTTPMiddleware):
    """相関ID採番 + 構造化アクセスログ。

    リクエストヘッダ X-Request-Id があれば踏襲、無ければ採番する。
    JSON-RPC の id は body 側にあるため、ここでは HTTP レベルの相関IDを扱う。
    """

    async def dispatch(self, request: Request, call_next):
        req_id = request.headers.get("X-Request-Id") or uuid.uuid4().hex
        request.state.request_id = req_id
        start = time.perf_counter()
        try:
            response: Response = await call_next(request)
        except Exception:
            elapsed = (time.perf_counter() - start) * 1000
            log.exception(
                "request failed request_id=%s method=%s path=%s elapsed_ms=%.1f",
                req_id, request.method, request.url.path, elapsed,
            )
            raise
        elapsed = (time.perf_counter() - start) * 1000
        log.info(
            "request_id=%s method=%s path=%s status=%s elapsed_ms=%.1f",
            req_id, request.method, request.url.path, response.status_code, elapsed,
        )
        response.headers["X-Request-Id"] = req_id
        return response
