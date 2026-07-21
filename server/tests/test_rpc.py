"""/rpc 結合テスト。設計書 00 の3章 JSON 例に対応。"""
from __future__ import annotations

from fastapi.testclient import TestClient

from app.main import app


def call(client, body):
    return client.post("/rpc", content=__import__("json").dumps(body))


def test_healthz():
    with TestClient(app) as client:
        r = client.get("/healthz")
        assert r.status_code == 200
        assert r.json() == {"status": "ok"}


def test_echo_ok():
    with TestClient(app) as client:
        r = call(client, {"jsonrpc": "2.0", "id": "1", "method": "echo",
                          "params": {"message": "hi"}})
        assert r.status_code == 200
        assert r.json() == {"jsonrpc": "2.0", "id": "1", "result": {"message": "hi"}}


def test_math_add():
    with TestClient(app) as client:
        r = call(client, {"jsonrpc": "2.0", "id": 2, "method": "math.add",
                          "params": {"a": 2, "b": 3}})
        assert r.json()["result"] == {"result": 5}


def test_domain_error_divide_by_zero():
    with TestClient(app) as client:
        r = call(client, {"jsonrpc": "2.0", "id": 3, "method": "math.div",
                          "params": {"a": 1, "b": 0}})
        body = r.json()
        assert body["error"]["code"] == 1001


def test_method_not_found():
    with TestClient(app) as client:
        r = call(client, {"jsonrpc": "2.0", "id": 4, "method": "nope"})
        assert r.json()["error"]["code"] == -32601


def test_invalid_params():
    with TestClient(app) as client:
        r = call(client, {"jsonrpc": "2.0", "id": 5, "method": "echo",
                          "params": {"wrong": 1}})
        assert r.json()["error"]["code"] == -32602


def test_parse_error():
    with TestClient(app) as client:
        r = client.post("/rpc", content="{ broken json ")
        assert r.status_code == 400
        assert r.json()["error"]["code"] == -32700


def test_notification_no_body():
    with TestClient(app) as client:
        r = call(client, {"jsonrpc": "2.0", "method": "echo",
                          "params": {"message": "x"}})
        assert r.status_code == 204
        assert r.content == b""


def test_methods_listing():
    with TestClient(app) as client:
        r = client.get("/rpc/methods")
        names = {m["method"] for m in r.json()["methods"]}
        assert {"echo", "math.add", "math.div", "sys.time", "sys.info"} <= names
