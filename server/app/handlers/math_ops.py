"""数値演算の例。ドメインエラー（0除算）を RpcException で返す例も示す。"""
from __future__ import annotations

from pydantic import BaseModel

from app.dispatcher import register
from app.protocol import RpcException

# ドメインエラーコード（1000〜 のアプリ固有域）
ERR_DIVIDE_BY_ZERO = 1001


class AddParams(BaseModel):
    a: float
    b: float


class DivParams(BaseModel):
    a: float
    b: float


@register("math.add", idempotent=True, params_model=AddParams)
async def add(p: AddParams) -> dict:
    return {"result": p.a + p.b}


@register("math.div", idempotent=True, params_model=DivParams)
async def div(p: DivParams) -> dict:
    if p.b == 0:
        raise RpcException(
            ERR_DIVIDE_BY_ZERO, "division by zero", data={"a": p.a, "b": p.b}
        )
    return {"result": p.a / p.b}
