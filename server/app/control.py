"""コントロールページ用のジョブキュー。

「次に実行するコマンドを挿入」= enqueue。ジョブは queued→running→done/error と
遷移する。実行経路は2つ:

  1. autorun（サーバ自動実行）: バックグラウンドループが queued を lease して
     ディスパッチャで実行 → ブラウザだけでデモが完結する。
  2. 外部ワーカ: lease/complete API を叩く外部プロセス（例: Go クライアント）が
     引き取って実行する。autorun を OFF にして使う。

すべて単一イベントループ上で動くため、各操作は await を挟まない同期クリティカル
セクションとし、ロックは用いない（実行 _execute のみ非同期）。
"""
from __future__ import annotations

import asyncio
import logging
import time
import uuid
from dataclasses import asdict, dataclass, field
from enum import Enum
from typing import Any

from app.config import settings
from app.dispatcher import dispatch, registry
from app.protocol import INTERNAL_ERROR, RpcException

log = logging.getLogger("csrpc.control")


class JobState(str, Enum):
    QUEUED = "queued"
    RUNNING = "running"
    DONE = "done"
    ERROR = "error"
    CANCELED = "canceled"


@dataclass
class Job:
    id: str
    seq: int
    method: str
    params: Any
    state: JobState
    created_at: float
    updated_at: float
    source: str = "web"          # 誰が挿入したか
    worker: str | None = None    # 誰が lease したか
    result: Any | None = None
    error: dict | None = None

    def to_dict(self) -> dict:
        d = asdict(self)
        d["state"] = self.state.value
        return d


class JobStore:
    def __init__(self) -> None:
        self._seq = 0
        self._jobs: dict[str, Job] = {}
        self._pending: list[str] = []          # queued の id（実行順）
        self._running: dict[str, Job] = {}
        self._history: list[Job] = []          # done/error/canceled（新しい順に前へ）
        self._autorun = settings.autorun
        self._tick = settings.autorun_tick
        self._task: asyncio.Task | None = None
        # asyncio プリミティブは start() 内（実行中ループ上）で生成する。
        # __init__ で作ると import 時のループに束縛され、別ループで使うと壊れる。
        self._stop: asyncio.Event | None = None

    # --- ライフサイクル ---
    async def start(self) -> None:
        self._stop = asyncio.Event()
        self._task = asyncio.create_task(self._run_loop(), name="autorun")

    async def stop(self) -> None:
        if self._stop is not None:
            self._stop.set()
        if self._task:
            await self._task
            self._task = None

    # --- 挿入・取得・完了 ---
    def enqueue(self, method: str, params: Any, source: str = "web") -> Job:
        self._seq += 1
        now = time.time()
        job = Job(
            id=uuid.uuid4().hex[:12],
            seq=self._seq,
            method=method,
            params=params,
            state=JobState.QUEUED,
            created_at=now,
            updated_at=now,
            source=source,
        )
        self._jobs[job.id] = job
        self._pending.append(job.id)
        log.info("enqueue seq=%d method=%s source=%s", job.seq, method, source)
        return job

    def lease(self, worker: str) -> Job | None:
        """先頭の queued を running にして返す。無ければ None。"""
        if not self._pending:
            return None
        jid = self._pending.pop(0)
        job = self._jobs[jid]
        job.state = JobState.RUNNING
        job.worker = worker
        job.updated_at = time.time()
        self._running[jid] = job
        return job

    def complete(self, job_id: str, result: Any = None, error: dict | None = None) -> Job | None:
        job = self._running.pop(job_id, None)
        if job is None:
            return None
        job.state = JobState.ERROR if error is not None else JobState.DONE
        job.result = result
        job.error = error
        job.updated_at = time.time()
        self._push_history(job)
        return job

    def cancel(self, job_id: str) -> bool:
        """queued のジョブのみキャンセル可能。"""
        if job_id in self._pending:
            self._pending.remove(job_id)
            job = self._jobs[job_id]
            job.state = JobState.CANCELED
            job.updated_at = time.time()
            self._push_history(job)
            return True
        return False

    def clear_history(self) -> None:
        self._history.clear()

    def _push_history(self, job: Job) -> None:
        self._history.insert(0, job)
        del self._history[settings.history_limit:]

    # --- 自動実行 ---
    @property
    def autorun(self) -> bool:
        return self._autorun

    def set_autorun(self, enabled: bool) -> None:
        self._autorun = enabled
        log.info("autorun=%s", enabled)

    async def step(self, worker: str = "server-step") -> Job | None:
        """queued を1件だけサーバ側で実行する（手動ステップ実行）。"""
        job = self.lease(worker)
        if job is None:
            return None
        await self._execute(job)
        return job

    async def _run_loop(self) -> None:
        while not self._stop.is_set():
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=self._tick)
                break  # stop がセットされた
            except asyncio.TimeoutError:
                pass
            if not self._autorun:
                continue
            job = self.lease("server-autorun")
            if job is not None:
                await self._execute(job)

    async def _execute(self, job: Job) -> None:
        try:
            result = await asyncio.wait_for(
                dispatch(job.method, job.params), timeout=settings.handler_timeout
            )
            self.complete(job.id, result=result)
        except RpcException as e:
            self.complete(job.id, error={"code": e.code, "message": e.message, "data": e.data})
        except asyncio.TimeoutError:
            self.complete(job.id, error={"code": INTERNAL_ERROR, "message": "Handler timed out"})
        except Exception:
            log.exception("job failed id=%s method=%s", job.id, job.method)
            self.complete(job.id, error={"code": INTERNAL_ERROR, "message": "Internal error"})

    # --- 表示用スナップショット ---
    def snapshot(self) -> dict:
        return {
            "autorun": self._autorun,
            "tick": self._tick,
            "methods": sorted(registry().keys()),
            "pending": [self._jobs[j].to_dict() for j in self._pending],
            "running": [j.to_dict() for j in self._running.values()],
            "history": [j.to_dict() for j in self._history],
        }


# シングルトン（registry と同じく import 副作用で共有）
store = JobStore()
