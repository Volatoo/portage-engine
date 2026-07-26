#!/usr/bin/env bash
set -euo pipefail

backup_type="${1:-full}"
case "${backup_type}" in
  full|diff|incr) ;;
  *) echo "backup type must be full, diff, or incr" >&2; exit 2 ;;
esac

compose=(docker compose -f docker-compose.yml -f docker-compose.pgbackrest.yml)
"${compose[@]}" exec -T --user postgres postgres \
  pgbackrest --stanza=portage-engine --type="${backup_type}" backup
"${compose[@]}" exec -T --user postgres postgres \
  pgbackrest --stanza=portage-engine info
