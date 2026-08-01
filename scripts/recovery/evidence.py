#!/usr/bin/env python3
"""Write deterministic, secret-minimizing Public Beta drill evidence."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
from typing import Any


SCHEMA_VERSION = 1


def digest_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def digest_file(path: str) -> str:
    source = Path(path)
    if not source.is_file():
        return ""
    digest = hashlib.sha256()
    with source.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def append_record(arguments: argparse.Namespace) -> None:
    record: dict[str, Any] = {
        "schema_version": SCHEMA_VERSION,
        "record_type": "check",
        "run_id": arguments.run_id,
        "gate": arguments.gate,
        "check_id": arguments.check_id,
        "execution": arguments.execution,
        "status": arguments.status,
        "destructive": arguments.destructive == "true",
        "started_at": arguments.started_at,
        "completed_at": arguments.completed_at,
        "duration_ms": arguments.duration_ms,
        "owner": arguments.owner,
        "isolated_target": arguments.target,
        "detail": arguments.detail,
        "command_label": arguments.command_label,
        "command_sha256": digest_text(arguments.command),
        "log_sha256": digest_file(arguments.log),
    }
    destination = Path(arguments.file)
    destination.parent.mkdir(parents=True, exist_ok=True)
    with destination.open("a", encoding="utf-8") as handle:
        json.dump(record, handle, sort_keys=True, separators=(",", ":"))
        handle.write("\n")


def finalize(arguments: argparse.Namespace) -> None:
    records_path = Path(arguments.file)
    records: list[dict[str, Any]] = []
    if records_path.is_file():
        with records_path.open(encoding="utf-8") as handle:
            for line in handle:
                if line.strip():
                    records.append(json.loads(line))
    statuses = [record["status"] for record in records]
    if arguments.status:
        status = arguments.status
    elif "failed" in statuses:
        status = "failed"
    elif records and all(value == "not-run" for value in statuses):
        status = "not-run"
    else:
        status = "passed"
    summary = {
        "schema_version": SCHEMA_VERSION,
        "evidence_kind": "portage-engine-public-beta-recovery-drill",
        "run_id": arguments.run_id,
        "gate": arguments.gate,
        "execution": arguments.execution,
        "status": status,
        "live_gate_status": "not-run" if arguments.execution == "static" else status,
        "owner": arguments.owner,
        "isolated_target": arguments.target,
        "baseline_commit": arguments.baseline_commit,
        "started_at": arguments.started_at,
        "completed_at": arguments.completed_at,
        "checks": records,
        "counts": {
            value: statuses.count(value)
            for value in ("passed", "failed", "not-run")
        },
    }
    destination = Path(arguments.output)
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_name(f".{destination.name}.tmp-{os.getpid()}")
    with temporary.open("w", encoding="utf-8") as handle:
        json.dump(summary, handle, indent=2, sort_keys=True)
        handle.write("\n")
    os.chmod(temporary, 0o600)
    os.replace(temporary, destination)


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser()
    commands = root.add_subparsers(dest="operation", required=True)
    append = commands.add_parser("append")
    for name in (
        "file", "run-id", "gate", "check-id", "execution", "status",
        "started-at", "completed-at", "owner", "target", "detail",
        "command-label", "command", "log",
    ):
        append.add_argument(f"--{name}", required=True)
    append.add_argument("--duration-ms", required=True, type=int)
    append.add_argument(
        "--destructive", choices=("true", "false"), required=True
    )
    append.set_defaults(function=append_record)

    final = commands.add_parser("finalize")
    for name in (
        "file", "output", "run-id", "gate", "execution", "owner",
        "target", "baseline-commit", "started-at", "completed-at",
    ):
        final.add_argument(f"--{name}", required=True)
    final.add_argument("--status", choices=("passed", "failed", "not-run"))
    final.set_defaults(function=finalize)
    return root


def main() -> None:
    arguments = parser().parse_args()
    arguments.function(arguments)


if __name__ == "__main__":
    main()
