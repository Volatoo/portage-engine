#!/usr/bin/env python3
"""Normalize an ISO-8601 PITR timestamp for pgBackRest's time parser."""

from __future__ import annotations

from datetime import datetime, timezone
import sys


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: pgbackrest-target-time.py <ISO-8601 timestamp>")

    raw = sys.argv[1].strip()
    if raw.endswith("Z"):
        raw = f"{raw[:-1]}+00:00"
    try:
        target = datetime.fromisoformat(raw)
    except ValueError as exc:
        raise SystemExit(f"invalid PITR target timestamp: {exc}") from exc
    if target.tzinfo is None:
        raise SystemExit("PITR target timestamp must include a timezone")

    # pgBackRest 2.59 does not accept the ISO-8601 T/Z spelling emitted by
    # PostgreSQL's to_char(), but it does accept this equivalent UTC form.
    print(target.astimezone(timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f+00"))


if __name__ == "__main__":
    main()
