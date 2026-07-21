"""コントロールページ / ジョブキューのテスト。"""
from __future__ import annotations

import time

from fastapi.testclient import TestClient

from app.main import app


def _disable_autorun(client):
    client.post("/control/autorun", json={"enabled": False})


def test_index_page():
    with TestClient(app) as client:
        r = client.get("/")
        assert r.status_code == 200
        assert "cs-rpc" in r.text


def test_run_now_ok():
    with TestClient(app) as client:
        r = client.post("/control/run-now",
                        json={"method": "echo", "params": {"message": "hi"}})
        d = r.json()
        assert d["ok"] is True
        assert d["result"] == {"message": "hi"}


def test_run_now_domain_error():
    with TestClient(app) as client:
        r = client.post("/control/run-now",
                        json={"method": "math.div", "params": {"a": 1, "b": 0}})
        d = r.json()
        assert d["ok"] is False
        assert d["error"]["code"] == 1001


def test_enqueue_unknown_method():
    with TestClient(app) as client:
        r = client.post("/control/enqueue", json={"method": "nope"})
        assert r.status_code == 400


def test_enqueue_then_step():
    with TestClient(app) as client:
        _disable_autorun(client)
        job = client.post("/control/enqueue",
                          json={"method": "echo", "params": {"message": "x"}}).json()
        jid = job["id"]

        pending = client.get("/control/state").json()["pending"]
        assert any(p["id"] == jid for p in pending)

        stepped = client.post("/control/step").json()
        assert stepped["id"] == jid
        assert stepped["state"] == "done"
        assert stepped["result"] == {"message": "x"}


def test_worker_lease_complete():
    with TestClient(app) as client:
        _disable_autorun(client)
        job = client.post("/control/enqueue",
                          json={"method": "sys.time"}).json()
        jid = job["id"]

        leased = client.post("/control/lease", json={"worker": "go-worker"}).json()
        assert leased["id"] == jid
        assert leased["state"] == "running"
        assert leased["worker"] == "go-worker"

        done = client.post("/control/complete",
                           json={"id": jid, "result": {"epoch": 1.0}}).json()
        assert done["state"] == "done"
        assert done["worker"] == "go-worker"


def test_cancel_queued():
    with TestClient(app) as client:
        _disable_autorun(client)
        jid = client.post("/control/enqueue",
                          json={"method": "echo", "params": {"message": "y"}}).json()["id"]
        r = client.post("/control/cancel", json={"id": jid})
        assert r.status_code == 200
        pending = client.get("/control/state").json()["pending"]
        assert all(p["id"] != jid for p in pending)


def test_autorun_executes():
    with TestClient(app) as client:
        client.post("/control/autorun", json={"enabled": True})
        jid = client.post("/control/enqueue",
                          json={"method": "math.add", "params": {"a": 1, "b": 2}}).json()["id"]
        # autorun ループ（tick 0.7s）が実行するのを待つ
        deadline = time.time() + 5
        done = None
        while time.time() < deadline:
            hist = client.get("/control/state").json()["history"]
            done = next((h for h in hist if h["id"] == jid), None)
            if done and done["state"] == "done":
                break
            time.sleep(0.2)
        assert done is not None and done["state"] == "done"
        assert done["result"] == {"result": 3}
