#!/usr/bin/env python3
"""Verify a locked external Portage profile repository and its exact parent chain."""

import json
import pathlib
import sys


def fail(message: str) -> None:
    raise SystemExit(message)


if len(sys.argv) != 5:
    fail("usage: verify-profile.py REPOSITORY_ROOT REPOSITORY_NAME PROFILE_PATH PARENTS_JSON")

root = pathlib.Path(sys.argv[1]).resolve(strict=True)
repository_name = sys.argv[2]
profile_path = sys.argv[3]
parents = json.loads(sys.argv[4])
if not isinstance(parents, list) or not parents:
    fail("external profile must declare a non-empty parent chain")

repo_name_path = root / "profiles" / "repo_name"
if repo_name_path.read_text(encoding="utf-8").strip() != repository_name:
    fail("profile repository identity does not match the locked plan")

profiles_root = (root / "profiles").resolve(strict=True)
selected = (profiles_root / profile_path).resolve(strict=True)
if not selected.is_dir() or selected == profiles_root or profiles_root not in selected.parents:
    fail("selected profile escapes the repository profiles directory")

actual = []
for line in (selected / "parent").read_text(encoding="utf-8").splitlines():
    line = line.strip()
    if line and not line.startswith("#"):
        actual.append(line)

expected = []
for parent in parents:
    if not isinstance(parent, dict) or set(parent) != {"repository", "profile_path"}:
        fail("profile parent evidence has an invalid shape")
    expected.append(f"{parent['repository']}:{parent['profile_path']}")
if actual != expected:
    fail(f"profile parent chain changed: got {actual!r}, expected {expected!r}")
