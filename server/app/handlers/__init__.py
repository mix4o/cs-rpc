"""ハンドラの自動 import。

このパッケージ配下の全モジュールを起動時に import することで、各モジュールの
`@register(...)` デコレータが実行され、レジストリへの登録が確定する。
新しいハンドラは handlers/ にファイルを置くだけでよい（この __init__ は不変）。
"""
from __future__ import annotations

import importlib
import pkgutil


def load_all() -> list[str]:
    """handlers 配下のモジュールを全て import し、モジュール名一覧を返す。"""
    loaded: list[str] = []
    for mod in pkgutil.iter_modules(__path__):
        if mod.name.startswith("_"):
            continue
        importlib.import_module(f"{__name__}.{mod.name}")
        loaded.append(mod.name)
    return loaded
