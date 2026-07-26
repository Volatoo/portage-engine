#!/usr/bin/env python3
"""Hydrate a digest-locked closure from allowlisted internal HTTP(S) URIs."""

from __future__ import annotations

import hashlib
import json
import os
import argparse
from pathlib import Path
import tempfile
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # type: ignore[no-untyped-def]
        return None


def digest(path: Path) -> tuple[int, str]:
    hasher = hashlib.sha256()
    size = 0
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            size += len(chunk)
            hasher.update(chunk)
    return size, hasher.hexdigest()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", type=Path, default=Path("/tmp/distfiles.MANIFEST.json"))
    parser.add_argument("--distdir", type=Path, default=Path("/var/cache/distfiles"))
    args = parser.parse_args()
    manifest = args.manifest
    distdir = args.distdir
    data = json.loads(manifest.read_text(encoding="utf-8"))
    objects = data.get("objects")
    if data.get("schema_version") != 1 or not isinstance(objects, list) or not objects:
        raise SystemExit("invalid distfile closure")
    distdir.mkdir(mode=0o755, parents=True, exist_ok=True)
    # Do not inherit HTTP(S)_PROXY. The manifest URI allowlist is meaningful
    # only when the connection goes directly to the reviewed internal mirror.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect)
    for item in objects:
        filename = item["filename"]
        expected_size = item["size"]
        expected_digest = item["sha256"]
        if Path(filename).name != filename:
            raise SystemExit(f"unsafe distfile name: {filename!r}")
        destination = distdir / filename
        if destination.is_file() and digest(destination) == (expected_size, expected_digest):
            continue
        descriptor, temporary = tempfile.mkstemp(prefix=".portage-engine-", dir=distdir)
        os.close(descriptor)
        temporary_path = Path(temporary)
        try:
            remaining = expected_size + 1
            with opener.open(item["uri"], timeout=120) as response, temporary_path.open("wb") as output:
                while remaining > 0 and (chunk := response.read(min(1024 * 1024, remaining))):
                    output.write(chunk)
                    remaining -= len(chunk)
            if digest(temporary_path) != (expected_size, expected_digest):
                raise SystemExit(f"distfile integrity mismatch: {filename}")
            temporary_path.chmod(0o644)
            temporary_path.replace(destination)
        finally:
            temporary_path.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
