"""echo: 受け取った message をそのまま返す最小サンプル。"""
from __future__ import annotations

from pydantic import BaseModel

from app.dispatcher import register


class EchoParams(BaseModel):
    message: str


@register("echo", idempotent=True, params_model=EchoParams)
async def echo(p: EchoParams) -> dict:
    return {"message": p.message}
