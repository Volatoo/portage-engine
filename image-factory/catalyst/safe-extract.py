#!/usr/bin/env python3
"""Extract a locked Catalyst runtime archive without path/device escapes."""

from __future__ import annotations

import argparse
from pathlib import Path, PurePosixPath
import shutil
import tarfile


def safe_path(name: str) -> PurePosixPath:
    path = PurePosixPath(name)
    if not name or path.is_absolute() or "\\" in name or ".." in path.parts:
        raise SystemExit(f"unsafe archive path: {name!r}")
    return path


def link_target(member: tarfile.TarInfo) -> PurePosixPath:
    target = PurePosixPath(member.linkname)
    if target.is_absolute() or "\\" in member.linkname:
        raise SystemExit(f"unsafe archive link: {member.name!r} -> {member.linkname!r}")
    combined = target if member.islnk() else PurePosixPath(member.name).parent / target
    parts: list[str] = []
    for part in combined.parts:
        if part in ("", "."):
            continue
        if part == "..":
            if not parts:
                raise SystemExit(f"archive link escapes root: {member.name!r}")
            parts.pop()
        else:
            parts.append(part)
    return PurePosixPath(*parts)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("archive", type=Path)
    parser.add_argument("destination", type=Path)
    args = parser.parse_args()
    destination = args.destination.resolve()
    destination.mkdir(mode=0o700, parents=True, exist_ok=False)

    with tarfile.open(args.archive, "r:*") as archive:
        members = archive.getmembers()
        if len(members) > 200_000:
            raise SystemExit("runtime archive contains too many entries")
        total = 0
        for member in members:
            safe_path(member.name)
            total += max(member.size, 0)
            if total > 2 * 1024 * 1024 * 1024:
                raise SystemExit("runtime archive expands beyond 2 GiB")
            if member.ischr() or member.isblk() or member.isfifo():
                raise SystemExit(f"special file rejected: {member.name!r}")
            if member.issym() or member.islnk():
                link_target(member)

        links: list[tarfile.TarInfo] = []
        for member in members:
            relative = safe_path(member.name)
            output = destination.joinpath(*relative.parts)
            if member.isdir():
                output.mkdir(mode=member.mode & 0o777, parents=True, exist_ok=True)
            elif member.isfile():
                output.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
                source = archive.extractfile(member)
                if source is None:
                    raise SystemExit(f"cannot read archive member: {member.name!r}")
                with source, output.open("wb") as stream:
                    shutil.copyfileobj(source, stream)
                output.chmod(member.mode & 0o777)
            elif member.issym() or member.islnk():
                links.append(member)
            else:
                raise SystemExit(f"unsupported archive entry: {member.name!r}")

        for member in links:
            relative = safe_path(member.name)
            output = destination.joinpath(*relative.parts)
            output.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
            target = link_target(member)
            if member.issym():
                output.symlink_to(member.linkname)
            else:
                source = destination.joinpath(*target.parts)
                if not source.is_file():
                    raise SystemExit(f"hardlink target is not a regular file: {member.linkname!r}")
                output.hardlink_to(source)


if __name__ == "__main__":
    main()
