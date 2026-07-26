#!/usr/bin/env python3
"""Fail if Catalyst imports third-party Python code outside its locked runtime."""

from __future__ import annotations

import importlib
from pathlib import Path
import sys
import sysconfig


def is_within(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
        return True
    except ValueError:
        return False


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit("usage: verify-runtime.py RUNTIME_SITE_PACKAGES RUNTIME_ROOT")
    runtime_site = Path(sys.argv[1]).resolve(strict=True)
    runtime_root = Path(sys.argv[2]).resolve(strict=True)
    if not is_within(runtime_site, runtime_root):
        raise SystemExit("runtime site-packages escapes the locked runtime")

    # The caller invokes this script with python -S. Keep the standard library
    # and locked runtime path, but remove host/user package directories.
    sys.path[:] = [str(runtime_site)] + [
        entry
        for entry in sys.path
        if entry
        and "site-packages" not in entry
        and "dist-packages" not in entry
        and Path(entry).resolve() != runtime_site
    ]
    # dev-python/pydecomp intentionally exports the historical, case-sensitive
    # DeComp package; Catalyst imports DeComp directly.
    required = ("catalyst", "DeComp", "fasteners", "gemato", "portage", "snakeoil", "tomli")
    for name in required:
        importlib.import_module(name)

    stdlib = Path(sysconfig.get_path("stdlib")).resolve(strict=True)
    verifier = Path(__file__).resolve(strict=True)
    violations: list[str] = []
    for name, module in sorted(sys.modules.items()):
        origin = getattr(module, "__file__", None)
        if not origin:
            continue
        path = Path(origin).resolve(strict=True)
        if name in {"__main__", "__mp_main__"} and path == verifier:
            continue
        if is_within(path, runtime_site) or (is_within(path, stdlib) and "site-packages" not in path.parts and "dist-packages" not in path.parts):
            continue
        violations.append(f"{name}={path}")
    if violations:
        raise SystemExit("runtime import escaped locked/stdlib roots: " + ", ".join(violations))
    print("CATALYST_RUNTIME_IMPORTS=LOCKED")


if __name__ == "__main__":
    main()
