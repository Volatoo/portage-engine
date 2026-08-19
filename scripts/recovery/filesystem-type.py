#!/usr/bin/env python3
"""Report the filesystem type containing a path on Linux or macOS."""

from __future__ import annotations

import platform
from pathlib import Path
import subprocess
import sys


def mount_table_type(target: Path) -> str:
    output = subprocess.run(
        ["mount"], check=True, capture_output=True, text=True
    ).stdout
    matches: list[tuple[int, str]] = []
    target_text = str(target)
    for line in output.splitlines():
        _, separator, mounted = line.rpartition(" on ")
        if not separator or " (" not in mounted:
            continue
        mount_point, options = mounted.rsplit(" (", 1)
        filesystem = options.split(",", 1)[0].rstrip(")").strip()
        mount_prefix = mount_point.rstrip("/") or "/"
        if target_text == mount_prefix or (
            mount_prefix == "/" or target_text.startswith(f"{mount_prefix}/")
        ):
            matches.append((len(mount_prefix), filesystem))
    if not matches:
        raise SystemExit(f"could not determine filesystem type for {target}")
    return max(matches)[1]


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: filesystem-type.py <path>")
    target = Path(sys.argv[1]).resolve(strict=True)
    if platform.system() == "Linux":
        filesystem = subprocess.run(
            ["stat", "-f", "-c", "%T", str(target)],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
    else:
        filesystem = mount_table_type(target)
    if not filesystem or filesystem in {"/", "@"}:
        raise SystemExit(f"invalid filesystem type for {target}: {filesystem!r}")
    print(filesystem)


if __name__ == "__main__":
    main()
