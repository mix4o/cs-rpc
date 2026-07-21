"""JSON-RPC 2.0 のモデルとエラー定義。

設計書 00_overview.md 3章 / 02_server_python.md 3章に対応。
"""
from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel

# --- エラーコード（全体設計 3.4 と一致） ---
PARSE_ERROR = -32700
INVALID_REQUEST = -32600
METHOD_NOT_FOUND = -32601
INVALID_PARAMS = -32602
INTERNAL_ERROR = -32603
# -32000〜-32099 はサーバ予約（将来: 認証失敗など）
# 1000〜 は各ハンドラのドメインエラー


class RpcError(BaseModel):
    code: int
    message: str
    data: Any | None = None


class RpcRequest(BaseModel):
    jsonrpc: Literal["2.0"]
    method: str
    id: str | int | None = None
    params: dict[str, Any] | list[Any] | None = None


class RpcResponse(BaseModel):
    jsonrpc: Literal["2.0"] = "2.0"
    id: str | int | None = None
    result: Any | None = None
    error: RpcError | None = None


class RpcException(Exception):
    """ハンドラ／ディスパッチャが投げる業務・プロトコルエラー。

    main.py がこれを捕捉して JSON-RPC の error 応答へ変換する。
    """

    def __init__(self, code: int, message: str, data: Any | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.data = data

    def to_error(self) -> RpcError:
        return RpcError(code=self.code, message=self.message, data=self.data)


def success(id_: str | int | None, result: Any) -> dict[str, Any]:
    return RpcResponse(id=id_, result=result).model_dump(exclude_none=True)


def failure(id_: str | int | None, code: int, message: str,
            data: Any | None = None) -> dict[str, Any]:
    return RpcResponse(
        id=id_, error=RpcError(code=code, message=message, data=data)
    ).model_dump(exclude_none=True)
