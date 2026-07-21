"""メソッド登録とディスパッチ。

`@register("method.name")` を付けるだけでレジストリに載る。
新コマンドの追加はハンドラ関数を1つ書くだけ（設計書 02 の4章）。
"""
from __future__ import annotations

import asyncio
import inspect
from dataclasses import dataclass
from typing import Any, Awaitable, Callable

from pydantic import BaseModel, ValidationError

from app.protocol import (
    INVALID_PARAMS,
    METHOD_NOT_FOUND,
    RpcException,
)

Handler = Callable[..., Any] | Callable[..., Awaitable[Any]]


@dataclass
class HandlerSpec:
    fn: Handler
    idempotent: bool
    params_model: type[BaseModel] | None

    def validate(self, params: Any) -> Any:
        """params を検証し、ハンドラに渡す引数へ変換する。

        - params_model 指定あり: dict を model_validate（無ければ空で生成）。
        - 指定なし: 生の params をそのまま渡す（None 可）。
        """
        if self.params_model is None:
            return params
        if params is None:
            payload: dict[str, Any] = {}
        elif isinstance(params, dict):
            payload = params
        else:  # list（位置引数）は現状未対応 → 明示エラー
            raise RpcException(
                INVALID_PARAMS,
                "Invalid params",
                data={"reason": "this method expects an object, not an array"},
            )
        try:
            return self.params_model.model_validate(payload)
        except ValidationError as e:
            raise RpcException(
                INVALID_PARAMS, "Invalid params", data=e.errors(include_url=False)
            ) from e

    async def call(self, args: Any) -> Any:
        """同期ハンドラはスレッドプールへ逃がしてループをブロックしない。"""
        if inspect.iscoroutinefunction(self.fn):
            return await self.fn(args)
        return await asyncio.to_thread(self.fn, args)


_REGISTRY: dict[str, HandlerSpec] = {}


def register(
    method: str,
    *,
    idempotent: bool = False,
    params_model: type[BaseModel] | None = None,
) -> Callable[[Handler], Handler]:
    def deco(fn: Handler) -> Handler:
        if method in _REGISTRY:
            raise RuntimeError(f"method already registered: {method}")
        _REGISTRY[method] = HandlerSpec(fn, idempotent, params_model)
        return fn

    return deco


async def dispatch(method: str, params: Any) -> Any:
    spec = _REGISTRY.get(method)
    if spec is None:
        raise RpcException(
            METHOD_NOT_FOUND, "Method not found", data={"method": method}
        )
    args = spec.validate(params)
    return await spec.call(args)


def registry() -> dict[str, HandlerSpec]:
    """登録済みメソッドの読み取り用ビュー（/rpc/methods で使用）。"""
    return dict(_REGISTRY)
