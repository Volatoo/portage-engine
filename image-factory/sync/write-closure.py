#!/usr/bin/env python3
"""Write a deterministic image-factory closure from a clean Portage DISTDIR."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import tempfile
from urllib.parse import quote, urlsplit


TARGET_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9+._/-]{0,255}$")
REVISION_RE = re.compile(r"^[a-f0-9]{40}$|^[a-f0-9]{64}$")


def parse_layout(distdir: Path) -> tuple[str, int] | None:
    layouts = sorted(distdir.glob(".layout.conf.*"))
    if len(layouts) != 1:
        raise SystemExit(f"expected exactly one mirror layout record, found {len(layouts)}")
    structure: str | None = None
    in_structure = False
    for raw_line in layouts[0].read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith(("#", ";")):
            continue
        if line.startswith("[") and line.endswith("]"):
            in_structure = line == "[structure]"
            continue
        if in_structure and line.startswith("0="):
            structure = line.partition("=")[2].strip()
            break
    if structure == "flat":
        return None
    match = re.fullmatch(r"filename-hash BLAKE2B ([0-9]+)", structure or "")
    if not match:
        raise SystemExit(f"unsupported primary mirror layout: {structure!r}")
    bits = int(match.group(1))
    if bits < 4 or bits > 512 or bits % 4:
        raise SystemExit(f"invalid BLAKE2B layout width: {bits}")
    return ("BLAKE2B", bits)


def object_uri(mirror: str, filename: str, layout: tuple[str, int] | None) -> str:
    path = ""
    if layout is not None:
        _, bits = layout
        path = hashlib.blake2b(filename.encode("utf-8")).hexdigest()[: bits // 4] + "/"
    return f"{mirror.rstrip('/')}/distfiles/{path}{quote(filename, safe='')}"


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--target", required=True)
    parser.add_argument("--repository-commit", required=True)
    parser.add_argument("--mirror", required=True)
    parser.add_argument("--distdir", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()

    endpoint = urlsplit(args.mirror)
    if (
        endpoint.scheme not in {"http", "https"}
        or not endpoint.hostname
        or endpoint.username is not None
        or endpoint.password is not None
        or endpoint.query
        or endpoint.fragment
    ):
        raise SystemExit("mirror must be an HTTP(S) endpoint without credentials, query, or fragment")
    if not TARGET_RE.fullmatch(args.target) or not REVISION_RE.fullmatch(args.repository_commit):
        raise SystemExit("invalid target or repository commit")
    if not args.distdir.is_dir():
        raise SystemExit("DISTDIR is not a directory")

    layout = parse_layout(args.distdir)
    objects: list[dict[str, object]] = []
    for path in sorted(args.distdir.iterdir(), key=lambda item: item.name):
        if path.name.startswith("."):
            continue
        if not path.is_file() or path.is_symlink() or path.name != Path(path.name).name:
            raise SystemExit(f"unsafe DISTDIR object: {path.name!r}")
        objects.append(
            {
                "filename": path.name,
                "uri": object_uri(args.mirror, path.name, layout),
                "sha256": sha256(path),
                "size": path.stat().st_size,
            }
        )
    if not objects:
        raise SystemExit("Portage produced an empty distfile closure")

    payload = {
        "schema_version": 1,
        "target": args.target,
        "repository_commit": args.repository_commit,
        "objects": objects,
    }
    args.output.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{args.output.name}.", dir=args.output.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(payload, stream, indent=2, sort_keys=False)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, 0o644)
        os.replace(temporary, args.output)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


if __name__ == "__main__":
    main()
