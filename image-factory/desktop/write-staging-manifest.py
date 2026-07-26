#!/usr/bin/env python3
"""Write the strict content manifest consumed by the Desktop E2E guest helper."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import tempfile


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("root", type=pathlib.Path)
    parser.add_argument("--output", default="MANIFEST.json")
    args = parser.parse_args()
    root = args.root.resolve(strict=True)
    if not root.is_dir():
        raise SystemExit("staging root must be a directory")
    output = root / args.output
    objects = []
    for path in sorted(root.rglob("*")):
        if path == output:
            continue
        if path.is_symlink():
            raise SystemExit(f"staging snapshot contains a symlink: {path}")
        if not path.is_file():
            continue
        relative = path.relative_to(root).as_posix()
        digest = hashlib.sha256()
        size = 0
        with path.open("rb") as stream:
            while chunk := stream.read(1024 * 1024):
                digest.update(chunk)
                size += len(chunk)
        objects.append({"path": relative, "size": size, "sha256": digest.hexdigest()})
    if not objects or not any(item["path"] == "Packages" for item in objects):
        raise SystemExit("staging snapshot must contain a Packages file")
    document = json.dumps({"schema_version": 1, "objects": objects}, indent=2, sort_keys=False).encode() + b"\n"
    descriptor, temporary_name = tempfile.mkstemp(prefix=".MANIFEST.", dir=root)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            os.fchmod(stream.fileno(), 0o644)
            stream.write(document)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary_name, output)
    finally:
        try:
            os.unlink(temporary_name)
        except FileNotFoundError:
            pass
    print("sha256:" + hashlib.sha256(document).hexdigest())


if __name__ == "__main__":
    main()
