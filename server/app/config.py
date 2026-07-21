"""サーバ設定。優先順位: 環境変数 > 既定値。

設計書 02_server_python.md 7章に対応。将来 .env 読み込みを足す場合も
ここに集約する。
"""
from __future__ import annotations

import os
from dataclasses import dataclass


def _env_int(name: str, default: int) -> int:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def _env_float(name: str, default: float) -> float:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    try:
        return float(raw)
    except ValueError:
        return default


def _env_bool(name: str, default: bool) -> bool:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    return raw.strip().lower() in ("1", "true", "yes", "on")


@dataclass(frozen=True)
class Settings:
    host: str = "127.0.0.1"
    port: int = 8080
    handler_timeout: int = 60          # ハンドラ実行の上限（秒）
    max_body: int = 10 * 1024 * 1024   # リクエストボディ上限（10MB）
    log_level: str = "INFO"
    # --- コントロールページ / ジョブキュー ---
    autorun: bool = True               # 起動時にサーバ自動実行を有効化するか
    autorun_tick: float = 0.7          # 自動実行ループの間隔（秒）＝状態遷移の可視化にも寄与
    history_limit: int = 100           # 実行履歴の保持件数

    @classmethod
    def from_env(cls) -> "Settings":
        return cls(
            host=os.getenv("CSRPC_HOST", cls.host),
            port=_env_int("CSRPC_PORT", cls.port),
            handler_timeout=_env_int("CSRPC_HANDLER_TIMEOUT", cls.handler_timeout),
            max_body=_env_int("CSRPC_MAX_BODY", cls.max_body),
            log_level=os.getenv("CSRPC_LOG_LEVEL", cls.log_level),
            autorun=_env_bool("CSRPC_AUTORUN", cls.autorun),
            autorun_tick=_env_float("CSRPC_AUTORUN_TICK", cls.autorun_tick),
            history_limit=_env_int("CSRPC_HISTORY_LIMIT", cls.history_limit),
        )


settings = Settings.from_env()
