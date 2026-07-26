#!/usr/bin/env python3
"""Verify a stage3 against hashes contained in its already-verified DIGESTS."""

from __future__ import annotations

import argparse
import hashlib
from pathlib import Path
import re


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("stage3", type=Path)
    parser.add_argument("digests", type=Path)
    args = parser.parse_args()
    filename = args.stage3.name
    if Path(filename).name != filename:
        raise SystemExit("unsafe stage3 filename")
    text = args.digests.read_text(encoding="utf-8", errors="strict")
    # Gentoo DIGESTS uses section headers. SHA512 and BLAKE2B are both 128
    # hexadecimal characters, so digest length alone cannot identify the
    # algorithm. Only accept an entry while inside the signed SHA512 section.
    sha512_candidates: list[str] = []
    section: str | None = None
    for line in text.splitlines():
        stripped = line.strip()
        header = re.fullmatch(r"#\s+([A-Z0-9]+)\s+HASH", stripped)
        if header:
            section = header.group(1)
            continue
        if stripped.startswith(("#", "-----")):
            section = None
            continue
        if section != "SHA512":
            continue
        match = re.fullmatch(r"([0-9a-fA-F]{128})\s+\*?(.+)", stripped)
        if match and match.group(2) == filename:
            sha512_candidates.append(match.group(1).lower())
    if len(sha512_candidates) != 1:
        raise SystemExit(
            f"signed DIGESTS must contain exactly one SHA512 entry for {filename}"
        )
    hasher = hashlib.sha512()
    with args.stage3.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            hasher.update(chunk)
    if hasher.hexdigest() != sha512_candidates[0]:
        raise SystemExit("stage3 SHA512 does not match signed DIGESTS")


if __name__ == "__main__":
    main()
