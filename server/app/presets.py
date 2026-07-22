"""プリセット: 複数コマンドをまとめて登録し、一括でキュー投入するための保存領域。

プリセットは JSON ファイルに永続化するのでサーバ再起動後も残る。
プリセット構造: {"name", "description", "commands": [{"method", "params"}, ...]}
"""
from __future__ import annotations

import json
import logging
import os
from typing import Any

from app.config import settings

log = logging.getLogger("csrpc.presets")


class PresetError(ValueError):
    """プリセット定義が不正なときに送出。"""


def _validate(preset: dict) -> dict:
    name = preset.get("name")
    if not isinstance(name, str) or not name.strip():
        raise PresetError("name is required")
    commands = preset.get("commands")
    if not isinstance(commands, list) or not commands:
        raise PresetError("commands must be a non-empty list")
    norm_cmds = []
    for i, c in enumerate(commands):
        if not isinstance(c, dict) or not isinstance(c.get("method"), str) or not c["method"]:
            raise PresetError(f"commands[{i}].method is required")
        params = c.get("params")
        if params is not None and not isinstance(params, (dict, list)):
            raise PresetError(f"commands[{i}].params must be object/array/null")
        norm_cmds.append({"method": c["method"], "params": params})
    return {
        "name": name.strip(),
        "description": str(preset.get("description") or ""),
        "commands": norm_cmds,
    }


class PresetStore:
    def __init__(self, path: str) -> None:
        self._path = path
        self._presets: dict[str, dict] = {}
        self._load()

    def _load(self) -> None:
        if not os.path.exists(self._path):
            return
        try:
            with open(self._path, encoding="utf-8") as f:
                data = json.load(f)
            for p in data.get("presets", []):
                try:
                    v = _validate(p)
                    self._presets[v["name"]] = v
                except PresetError:
                    continue
            log.info("loaded %d presets from %s", len(self._presets), self._path)
        except (OSError, json.JSONDecodeError) as e:
            log.warning("could not load presets: %s", e)

    def _save(self) -> None:
        tmp = self._path + ".tmp"
        try:
            with open(tmp, "w", encoding="utf-8") as f:
                json.dump({"presets": list(self._presets.values())}, f,
                          ensure_ascii=False, indent=2)
            os.replace(tmp, self._path)
        except OSError as e:
            log.warning("could not save presets: %s", e)

    def list(self) -> list[dict]:
        return sorted(self._presets.values(), key=lambda p: p["name"])

    def get(self, name: str) -> dict | None:
        return self._presets.get(name)

    def put(self, preset: dict) -> dict:
        v = _validate(preset)
        self._presets[v["name"]] = v
        self._save()
        return v

    def delete(self, name: str) -> bool:
        if name in self._presets:
            del self._presets[name]
            self._save()
            return True
        return False


# シングルトン（他モジュールから共有）
presets = PresetStore(settings.presets_file)
