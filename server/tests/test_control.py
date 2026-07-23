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


def _use_temp_presets(tmp_path):
    from app import web
    web.presets._dir = str(tmp_path / "presets")
    web.presets._presets = {}
    web.presets._files = {}
    return web


def test_presets_crud_and_run(tmp_path):
    web = _use_temp_presets(tmp_path)
    with TestClient(app) as client:
        # サーバ実行(autorun)で逐次実行を進める。echo は server 実行可能。
        client.post("/control/autorun", json={"enabled": True})
        preset = {
            "name": "demo",
            "description": "echo → 待機 → echo",
            "commands": [
                {"method": "echo", "params": {"message": "a"}},
                {"wait": 0.1},  # 制御ステップ: クライアントに送らずサーバ側で待機
                {"method": "echo", "params": {"message": "b"}},
            ],
        }
        assert client.post("/control/presets", json=preset).status_code == 200
        assert (tmp_path / "presets" / "demo.json").exists()

        r = client.post("/control/presets/run", json={"name": "demo"}).json()
        assert r["started"] and r["commands"] == 2 and r["waits"] == 1

        # 逐次実行の完了を待つ（履歴に demo 由来の 2 コマンドが入る）
        got = 0
        deadline = time.time() + 6
        while time.time() < deadline:
            hist = client.get("/control/state").json()["history"]
            got = sum(1 for j in hist if j.get("source") == "preset:demo")
            if got >= 2:
                break
            time.sleep(0.1)
        assert got == 2

        assert client.post("/control/presets/delete", json={"name": "demo"}).status_code == 200
        assert client.post("/control/presets/run", json={"name": "demo"}).status_code == 404


def test_preset_rejects_negative_wait(tmp_path):
    _use_temp_presets(tmp_path)
    with TestClient(app) as client:
        r = client.post("/control/presets", json={
            "name": "bad", "commands": [{"wait": -1}]})
        assert r.status_code == 400


def test_preset_validation_rejects_empty_commands(tmp_path):
    _use_temp_presets(tmp_path)
    with TestClient(app) as client:
        r = client.post("/control/presets", json={"name": "x", "commands": []})
        assert r.status_code in (400, 422)


def test_presets_load_one_file_per_preset(tmp_path):
    import json as _json
    web = _use_temp_presets(tmp_path)
    d = tmp_path / "presets"
    d.mkdir()
    # name 省略 → ファイル名がプリセット名になる
    (d / "recon.json").write_text(_json.dumps(
        {"description": "info", "commands": [{"method": "sys.info", "params": None}]}),
        encoding="utf-8")
    assert web.presets.reload() == 1
    with TestClient(app) as client:
        presets = client.get("/control/presets").json()["presets"]
        assert presets[0]["name"] == "recon"
        assert presets[0]["commands"][0]["method"] == "sys.info"


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
