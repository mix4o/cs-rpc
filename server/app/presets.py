"""プリセット: 複数コマンドをまとめて登録し、一括でキュー投入するための保存領域。

**1プリセット = 1ファイル**。ディレクトリ（CSRPC_PRESETS_DIR、既定 `presets/`）配下に
`<名前>.json` を置く。ファイルを直接用意して並べるだけで準備でき、Web UI からの保存も
そのディレクトリに個別ファイルとして書き出す。

1ファイルの構造:
    {"name": "...", "description": "...", "commands": [{"method", "params"}, ...]}
`name` を省略した場合はファイル名（拡張子を除いた部分）がプリセット名になる。
"""
from __future__ import annotations

import glob
import json
import logging
import os
import re

from app.config import settings

log = logging.getLogger("csrpc.presets")

_SAFE = re.compile(r"[^A-Za-z0-9._-]")


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
        if not isinstance(c, dict):
            raise PresetError(f"commands[{i}] must be an object")
        # 制御ステップ: {"wait": 秒} はクライアントに送らずサーバ側で待機する
        if "wait" in c:
            w = c["wait"]
            if not isinstance(w, (int, float)) or isinstance(w, bool) or w < 0:
                raise PresetError(f"commands[{i}].wait must be a non-negative number")
            norm_cmds.append({"wait": float(w)})
            continue
        if not isinstance(c.get("method"), str) or not c["method"]:
            raise PresetError(f"commands[{i}] needs 'method' (command) or 'wait' (control)")
        params = c.get("params")
        if params is not None and not isinstance(params, (dict, list)):
            raise PresetError(f"commands[{i}].params must be object/array/null")
        norm_cmds.append({"method": c["method"], "params": params})
    return {
        "name": name.strip(),
        "description": str(preset.get("description") or ""),
        "commands": norm_cmds,
    }


def _safe_filename(name: str) -> str:
    return (_SAFE.sub("_", name) or "preset") + ".json"


class PresetStore:
    def __init__(self, directory: str) -> None:
        self._dir = directory
        self._presets: dict[str, dict] = {}
        self._files: dict[str, str] = {}  # name -> ファイルパス
        self._load()

    def _load(self) -> None:
        if not os.path.isdir(self._dir):
            return
        for path in sorted(glob.glob(os.path.join(self._dir, "*.json"))):
            try:
                with open(path, encoding="utf-8") as f:
                    data = json.load(f)
            except (OSError, json.JSONDecodeError) as e:
                log.warning("skip %s: %s", path, e)
                continue
            # name 省略時はファイル名（拡張子なし）をプリセット名にする
            if isinstance(data, dict) and not data.get("name"):
                data = {**data, "name": os.path.splitext(os.path.basename(path))[0]}
            try:
                v = _validate(data)
            except PresetError as e:
                log.warning("skip %s: %s", path, e)
                continue
            self._presets[v["name"]] = v
            self._files[v["name"]] = path
        log.info("loaded %d presets from %s/", len(self._presets), self._dir)

    def list(self) -> list[dict]:
        return sorted(self._presets.values(), key=lambda p: p["name"])

    def get(self, name: str) -> dict | None:
        return self._presets.get(name)

    def put(self, preset: dict) -> dict:
        v = _validate(preset)
        # 既存の読み込み元があればそのファイルを、無ければ名前から新規ファイルを使う
        path = self._files.get(v["name"]) or os.path.join(self._dir, _safe_filename(v["name"]))
        os.makedirs(self._dir, exist_ok=True)
        tmp = path + ".tmp"
        try:
            with open(tmp, "w", encoding="utf-8") as f:
                json.dump(v, f, ensure_ascii=False, indent=2)
            os.replace(tmp, path)
        except OSError as e:
            raise PresetError(f"could not write preset file: {e}")
        self._presets[v["name"]] = v
        self._files[v["name"]] = path
        return v

    def delete(self, name: str) -> bool:
        if name not in self._presets:
            return False
        path = self._files.pop(name, None)
        del self._presets[name]
        if path:
            try:
                os.remove(path)
            except OSError:
                pass
        return True

    def reload(self) -> int:
        """ディレクトリを再スキャンして読み直す（ファイル差し替え後の反映用）。"""
        self._presets = {}
        self._files = {}
        self._load()
        return len(self._presets)


# シングルトン（他モジュールから共有）
presets = PresetStore(settings.presets_dir)
