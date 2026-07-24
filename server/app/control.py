"""コントロールページ用のジョブキュー + 接続クライアント（ワーカ）レジストリ。

「次に実行するコマンドを挿入」= enqueue。ジョブは queued→running→done/error と
遷移する。実行経路は2つ:

  1. autorun（サーバ自動実行）: バックグラウンドループが queued を lease して
     ディスパッチャで実行 → ブラウザだけでデモが完結する。
  2. 外部ワーカ: lease/complete API を叩く外部プロセス（例: Go クライアント）が
     引き取って実行する。autorun を OFF にして使う。

複数の外部ワーカが同時に接続できる。HTTP はステートレスなので「接続」の実体は
無いが、announce / lease / progress / complete の各呼び出しを heartbeat として
扱い、ワーカ名ごとに last_seen・申告メソッド・実行中/完了数を記録する。lease の
ポーリング自体が heartbeat になるため、ジョブが無くても接続は生きたままになる。
last_seen が settings.client_timeout を超えたクライアントは offline 扱い。

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
class Client:
    """接続中のワーカ1台の状態。名前（worker）を一意キーとする。"""
    name: str
    methods: list[str] = field(default_factory=list)  # announce で申告した実行可能メソッド
    first_seen: float = 0.0
    last_seen: float = 0.0            # 直近の heartbeat（announce/lease/progress/complete）
    leased: int = 0                  # 現在 lease 中（未 complete）のジョブ数
    completed: int = 0               # complete 報告した累計
    last_job: str | None = None      # 直近に lease したジョブ id
    last_method: str | None = None   # 直近に lease したメソッド

    def to_dict(self, now: float, timeout: float) -> dict:
        d = asdict(self)
        d["online"] = (now - self.last_seen) <= timeout
        d["idle_ms"] = int(max(0.0, now - self.last_seen) * 1000)
        return d


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
    target: str | None = None    # 宛先ワーカ名。None=誰でも取れる（共有）／指定=そのワーカ専用
    group: str | None = None     # 同一ブロードキャストを束ねる id（None=単発）
    result: Any | None = None
    error: dict | None = None
    progress: dict | None = None       # 実行中の途中経過（例: {scanned, matched}）
    cancel_requested: bool = False     # コントロールページからの中断要求

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
        self._worker_methods: set[str] = set() # ワーカが申告した実行可能メソッド（和集合）
        self._clients: dict[str, Client] = {}  # worker名 -> 接続クライアント状態
        self._client_timeout = settings.client_timeout
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
    def enqueue(self, method: str, params: Any, source: str = "web",
                target: str | None = None, group: str | None = None) -> Job:
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
            target=target,
            group=group,
        )
        self._jobs[job.id] = job
        self._pending.append(job.id)
        log.info("enqueue seq=%d method=%s source=%s target=%s",
                 job.seq, method, source, target or "*")
        return job

    def broadcast(self, method: str, params: Any, source: str = "broadcast",
                  online_only: bool = True) -> list[Job]:
        """接続中の各クライアント宛に同じ命令を1つずつ複製投入する（ファンアウト）。

        呼び出し時点のクライアント一覧のスナップショットに対して投入し、即座に返す
        （ファイア・アンド・フォーゲット）。以降に接続したクライアントは対象外。
        online_only=True なら heartbeat が生きているワーカだけを対象にする。
        戻り値は生成したジョブ列（0台なら空）。
        """
        now = time.time()
        targets = sorted(
            c.name for c in self._clients.values()
            if not online_only or (now - c.last_seen) <= self._client_timeout
        )
        group = uuid.uuid4().hex[:8]
        jobs = [self.enqueue(method, params, source=source, target=t, group=group)
                for t in targets]
        log.info("broadcast group=%s method=%s targets=%d", group, method, len(jobs))
        return jobs

    def _next_pending_for(self, worker: str) -> Job | None:
        """worker が取れる先頭ジョブを返す: 宛先なし（共有）または自分宛の先頭。"""
        for jid in self._pending:
            job = self._jobs[jid]
            if job.target is None or job.target == worker:
                return job
        return None

    def lease(self, worker: str, *, track: bool = True) -> Job | None:
        """先頭の queued を running にして返す。無ければ None。

        track=True（外部ワーカ）のとき、この呼び出しをクライアントの heartbeat と
        して扱う。ジョブが無い（None を返す）ポーリングでも接続は生きたまま。
        track=False はサーバ内部実行（step/autorun）用でクライアント登録しない。
        """
        client = self.touch_client(worker) if track else None
        job = self._next_pending_for(worker)
        if job is None:
            return None
        self._pending.remove(job.id)
        job.state = JobState.RUNNING
        job.worker = worker
        job.updated_at = time.time()
        self._running[job.id] = job
        if client is not None:
            client.leased += 1
            client.last_job = job.id
            client.last_method = job.method
        return job

    def complete(self, job_id: str, result: Any = None, error: dict | None = None,
                 canceled: bool = False) -> Job | None:
        job = self._running.pop(job_id, None)
        if job is None:
            return None
        if canceled:
            job.state = JobState.CANCELED
        elif error is not None:
            job.state = JobState.ERROR
        else:
            job.state = JobState.DONE
        job.result = result
        job.error = error
        job.updated_at = time.time()
        # ジョブを引き取ったクライアントの統計を更新（heartbeat も兼ねる）。
        # サーバ内部実行(server-*)は登録されていないので None になり無視される。
        client = self._clients.get(job.worker) if job.worker else None
        if client is not None:
            client.leased = max(0, client.leased - 1)
            client.completed += 1
            client.last_seen = time.time()
        self._push_history(job)
        return job

    def report_progress(self, job_id: str, progress: dict) -> bool:
        """実行中ジョブの途中経過を更新し、中断要求の有無を返す。

        ワーカはこの戻り値（cancel_requested）を見て実行を中断する。
        """
        job = self._running.get(job_id)
        if job is None:
            return False
        job.progress = progress
        job.updated_at = time.time()
        client = self._clients.get(job.worker) if job.worker else None
        if client is not None:
            client.last_seen = time.time()
        return job.cancel_requested

    def cancel(self, job_id: str) -> str:
        """queued はその場でキャンセル、running は中断要求を立てる。

        戻り値: "canceled"（queued を除去） / "cancel_requested"（running へ要求） /
                "not_found"。
        """
        if job_id in self._pending:
            self._pending.remove(job_id)
            job = self._jobs[job_id]
            job.state = JobState.CANCELED
            job.updated_at = time.time()
            self._push_history(job)
            return "canceled"
        job = self._running.get(job_id)
        if job is not None:
            job.cancel_requested = True
            job.updated_at = time.time()
            return "cancel_requested"
        return "not_found"

    def announce(self, worker: str, methods: list[str]) -> Client:
        """ワーカが接続を申告する: クライアント登録 + 実行可能メソッドの申告。

        methods はコントロールページの選択肢（既知メソッド集合）に反映され、
        worker はクライアント一覧に載る（heartbeat も更新）。
        """
        self._worker_methods.update(methods)
        return self.touch_client(worker, methods=methods)

    def known_methods(self) -> set[str]:
        """サーバ登録 + ワーカ申告のメソッド集合（enqueue 検証・一覧表示に使う）。"""
        return set(registry().keys()) | self._worker_methods

    # --- 接続クライアント（ワーカ）レジストリ ---
    def touch_client(self, name: str, methods: list[str] | None = None) -> Client:
        """クライアントの heartbeat を更新する（無ければ新規登録）。"""
        now = time.time()
        client = self._clients.get(name)
        if client is None:
            client = Client(name=name, first_seen=now, last_seen=now)
            self._clients[name] = client
            log.info("client connected name=%s", name)
        client.last_seen = now
        if methods is not None:
            client.methods = sorted(set(methods))
        return client

    def clients(self) -> list[dict]:
        """接続クライアント一覧（online/offline を last_seen から算出）。名前順。"""
        now = time.time()
        return [c.to_dict(now, self._client_timeout)
                for c in sorted(self._clients.values(), key=lambda c: c.name)]

    def forget_client(self, name: str) -> bool:
        """クライアントを一覧から除去する（明示切断・掃除用）。"""
        return self._clients.pop(name, None) is not None

    def prune_offline_clients(self) -> int:
        """offline のクライアントを一覧から除去し、除去数を返す。"""
        now = time.time()
        stale = [n for n, c in self._clients.items()
                 if (now - c.last_seen) > self._client_timeout]
        for n in stale:
            del self._clients[n]
        return len(stale)

    def get_job(self, job_id: str) -> Job | None:
        """id からジョブを引く（プリセット逐次実行の完了待ちに使う）。"""
        return self._jobs.get(job_id)

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
        job = self.lease(worker, track=False)  # サーバ内部実行はクライアント登録しない
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
            job = self.lease("server-autorun", track=False)
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
            "methods": sorted(self.known_methods()),
            "clients": self.clients(),
            "client_timeout": self._client_timeout,
            "pending": [self._jobs[j].to_dict() for j in self._pending],
            "running": [j.to_dict() for j in self._running.values()],
            "history": [j.to_dict() for j in self._history],
        }


# シングルトン（registry と同じく import 副作用で共有）
store = JobStore()
