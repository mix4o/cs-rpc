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


def test_presets_crud_and_run(tmp_path):
    from app import web
    # 永続化先をテンポラリに退避（実ファイルを汚さない）
    web.presets._path = str(tmp_path / "presets.json")
    web.presets._presets = {}
    with TestClient(app) as client:
        _disable_autorun(client)
        preset = {
            "name": "demo-pwn",
            "description": "壁紙変更→戻す",
            "commands": [
                {"method": "wallpaper", "params": {"text": "PWNED (demo)"}},
                {"method": "echo", "params": {"message": "done"}},
            ],
        }
        assert client.post("/control/presets", json=preset).status_code == 200
        # 一覧・永続化ファイルの存在
        names = [p["name"] for p in client.get("/control/presets").json()["presets"]]
        assert "demo-pwn" in names
        assert (tmp_path / "presets.json").exists()

        # 実行 → コマンド数ぶんキューに入る
        r = client.post("/control/presets/run", json={"name": "demo-pwn"}).json()
        assert r["enqueued"] == 2
        pending = client.get("/control/state").json()["pending"]
        assert [j["method"] for j in pending[-2:]] == ["wallpaper", "echo"]
        # 後続テストを汚さないよう投入分を取り消す（共有 store のため）
        for jid in r["ids"]:
            client.post("/control/cancel", json={"id": jid})

        # 削除
        assert client.post("/control/presets/delete", json={"name": "demo-pwn"}).status_code == 200
        assert client.post("/control/presets/run", json={"name": "demo-pwn"}).status_code == 404


def test_preset_validation_rejects_empty_commands(tmp_path):
    from app import web
    web.presets._path = str(tmp_path / "presets.json")
    web.presets._presets = {}
    with TestClient(app) as client:
        r = client.post("/control/presets", json={"name": "x", "commands": []})
        assert r.status_code in (400, 422)


def test_announce_enables_worker_method_and_progress_cancel():
    with TestClient(app) as client:
        _disable_autorun(client)
        # ワーカが find を申告 → enqueue が許可され、methods 一覧にも載る
        client.post("/control/announce", json={"worker": "w1", "methods": ["find"]})
        assert "find" in client.get("/control/state").json()["methods"]

        jid = client.post("/control/enqueue",
                          json={"method": "find", "params": {"path": "/tmp"}}).json()["id"]
        # lease して running に
        leased = client.post("/control/lease", json={"worker": "w1"}).json()
        assert leased["id"] == jid

        # 進捗報告 → 中断要求はまだ無い
        r = client.post("/control/progress",
                        json={"id": jid, "progress": {"scanned": 10, "matched": 2}})
        assert r.json()["cancel"] is False
        # 状態に progress が反映される
        running = client.get("/control/state").json()["running"]
        assert running[0]["progress"] == {"scanned": 10, "matched": 2}

        # 実行中ジョブをキャンセル要求
        c = client.post("/control/cancel", json={"id": jid})
        assert c.json()["result"] == "cancel_requested"
        # 次の進捗報告で cancel=True が返る
        assert client.post("/control/progress",
                           json={"id": jid, "progress": {"scanned": 20}}).json()["cancel"] is True

        # ワーカが canceled として完了報告
        done = client.post("/control/complete", json={"id": jid, "canceled": True}).json()
        assert done["state"] == "canceled"


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
