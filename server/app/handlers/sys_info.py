"""sys.time / sys.info: サーバ状態を返す。params 不要な同期ハンドラの例。

同期関数（非 async）として定義しており、ディスパッチャがスレッドプールで実行する。
"""
from __future__ import annotations

import platform
import time

from app.dispatcher import register


@register("sys.time", idempotent=True)
def sys_time(_params) -> dict:
    return {"epoch": time.time(), "iso": time.strftime("%Y-%m-%dT%H:%M:%S%z")}


@register("sys.info", idempotent=True)
def sys_info(_params) -> dict:
    return {
        "python": platform.python_version(),
        "system": platform.system(),
        "release": platform.release(),
        "machine": platform.machine(),
    }
