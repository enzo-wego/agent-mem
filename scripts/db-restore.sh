#!/usr/bin/env bash
# Restore an agent-mem custom-format dump into the compose Postgres service.
#
# The database is DROPPED and recreated, not restored over the top of itself.
# Restoring into a populated database leaves the old rows in place wherever the
# dump has no matching key, which silently yields a hybrid of two snapshots —
# the worst possible outcome for a migration you are about to trust.
#
# Because every index is rebuilt from its CREATE INDEX statement, this is also
# the repair path for a corrupt HNSW index: there is no way for corruption in
# the source index pages to travel through a logical dump.
#
# The worker is stopped before the restore and deliberately left stopped
# afterwards. On a migration you want to inspect the restored data before
# anything starts writing to it — and, more sharply, before a second worker
# starts answering the same Slack events as the one still running elsewhere.
# Pass --start-worker to override.
#
# Usage:
#   scripts/db-restore.sh backups/agentmem-20260812-171500.dump
#   scripts/db-restore.sh --force backups/....dump      # clobber a populated DB
#   scripts/db-restore.sh --start-worker backups/....dump
#   DOCKER="sudo docker" scripts/db-restore.sh ...      # Linux root-owned socket
set -euo pipefail

DOCKER="${DOCKER:-docker}"
COMPOSE="${COMPOSE:-$DOCKER compose}"
SERVICE="${SERVICE:-postgres}"
WORKER_SERVICE="${WORKER_SERVICE:-worker}"

PGUSER="${PGUSER:-agentmem}"
PGDATABASE="${PGDATABASE:-agentmem}"
PG_IMAGE="${PG_IMAGE:-pgvector/pgvector:pg16}"

# Must match POSTGRES_PASSWORD in docker-compose.yml, which defaults the same way.
PGPASSWORD="${AGENT_MEM_PG_PASSWORD:-agentmem}"

# Parallel restore jobs. Index builds dominate the wall clock here and they
# parallelise well; 4 matches the colima VM's CPU allocation on payments.
JOBS="${JOBS:-4}"

force=0
start_worker=0
dump=""
while [ $# -gt 0 ]; do
  case "$1" in
    --force)        force=1 ;;
    --start-worker) start_worker=1 ;;
    -h|--help)      sed -n '2,28p' "$0"; exit 0 ;;
    -*)             echo "unknown flag: $1" >&2; exit 2 ;;
    *)              dump="$1" ;;
  esac
  shift
done

[ -n "$dump" ] || { echo "usage: $0 [--force] [--start-worker] <dump-file>" >&2; exit 2; }
[ -f "$dump" ]  || { echo "!! no such dump: $dump" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# pg_restore seeks within the archive, so it must see a real file, not a pipe.
# The directory is bind-mounted into the throwaway client container below.
dump_dir="$(cd "$(dirname "$dump")" && pwd)"
dump_file="$(basename "$dump")"

echo ">> bringing up '$SERVICE'"
$COMPOSE up -d "$SERVICE"

# Wait for readiness rather than assuming: a restore fired at a still-starting
# server fails halfway and leaves a half-populated database behind.
echo -n ">> waiting for postgres"
for _ in $(seq 1 60); do
  if $COMPOSE exec -T "$SERVICE" pg_isready -U "$PGUSER" >/dev/null 2>&1; then break; fi
  echo -n "."
  sleep 1
done
echo
$COMPOSE exec -T "$SERVICE" pg_isready -U "$PGUSER" >/dev/null 2>&1 \
  || { echo "!! postgres did not become ready" >&2; exit 1; }

psql_super() { $COMPOSE exec -T "$SERVICE" psql -U "$PGUSER" -d postgres -v ON_ERROR_STOP=1 "$@"; }

# Refuse to destroy a populated database unless explicitly told to. "Populated"
# means user tables exist, not merely that the database does.
existing="$($COMPOSE exec -T "$SERVICE" psql -U "$PGUSER" -d "$PGDATABASE" -tAc \
  "SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema')" \
  2>/dev/null || echo 0)"
existing="$(printf '%s' "$existing" | tr -dc '0-9')"
if [ "${existing:-0}" -gt 0 ] && [ "$force" -ne 1 ]; then
  echo "!! '$PGDATABASE' already holds ${existing} tables. Re-run with --force to drop and replace it." >&2
  exit 1
fi

echo ">> stopping '$WORKER_SERVICE' so nothing writes during the restore"
$COMPOSE stop "$WORKER_SERVICE" 2>/dev/null || true

# DROP DATABASE fails while any session is attached, and the worker is not the
# only possible client (a psql left open in another window is enough).
echo ">> dropping and recreating '$PGDATABASE'"
psql_super -c \
  "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$PGDATABASE' AND pid <> pg_backend_pid()" \
  >/dev/null
psql_super -c "DROP DATABASE IF EXISTS $PGDATABASE"
psql_super -c "CREATE DATABASE $PGDATABASE OWNER $PGUSER"

echo ">> restoring $dump_file (${JOBS} parallel jobs; HNSW index builds dominate)"
# Joining the server container's network namespace means this client reaches it
# on 127.0.0.1 without needing to know the compose network's name.
pg_cid="$($COMPOSE ps -q "$SERVICE")"
[ -n "$pg_cid" ] || { echo "!! could not resolve the $SERVICE container" >&2; exit 1; }

# --no-owner/--no-privileges: object ownership is re-derived from the connecting
# role, so a dump taken on a host whose roles differ still restores cleanly.
# Deliberately NOT allowed to abort the script under `set -e`. pg_restore exits
# non-zero for a single ignored error, and dying here would skip the
# verification below — which is exactly the moment you most need it to run and
# tell you what is missing.
restore_rc=0
$DOCKER run --rm \
  -v "$dump_dir":/backups:ro \
  --network "container:$pg_cid" \
  -e PGPASSWORD="$PGPASSWORD" \
  "$PG_IMAGE" \
  pg_restore -h 127.0.0.1 -U "$PGUSER" -d "$PGDATABASE" \
    --no-owner --no-privileges --jobs "$JOBS" --verbose \
    "/backups/$dump_file" || restore_rc=$?

[ "$restore_rc" -eq 0 ] || echo "!! pg_restore exited $restore_rc — verifying what landed before failing" >&2

echo
echo ">> post-restore verification"
$COMPOSE exec -T "$SERVICE" psql -U "$PGUSER" -d "$PGDATABASE" -c "\dx"
$COMPOSE exec -T "$SERVICE" psql -U "$PGUSER" -d "$PGDATABASE" -c \
  "SELECT schemaname, relname, n_live_tup FROM pg_stat_user_tables ORDER BY n_live_tup DESC LIMIT 10"

# An HNSW index that failed to build leaves the table queryable but every vector
# search silently degrades to a sequential scan, so count them explicitly.
$COMPOSE exec -T "$SERVICE" psql -U "$PGUSER" -d "$PGDATABASE" -tAc \
  "SELECT 'hnsw indexes: ' || count(*) FROM pg_indexes WHERE indexdef ILIKE '%USING hnsw%'"

# pg_restore exits 0 even when individual objects failed, so surface anything
# left invalid rather than trusting the exit code.
invalid="$($COMPOSE exec -T "$SERVICE" psql -U "$PGUSER" -d "$PGDATABASE" -tAc \
  "SELECT count(*) FROM pg_index WHERE NOT indisvalid" | tr -dc '0-9')"

# An index whose CREATE failed outright does not exist at all, so it is invisible
# to the check above — it is not invalid, it is absent. This bit the migration:
# a parallel HNSW build on public.observations exhausted the container's 64 MB
# /dev/shm, pg_restore logged it as an ignored error, and the primary vector
# index was silently missing from an otherwise perfect-looking restore.
# So compare against what the archive actually promised.
expected="$($DOCKER run --rm -v "$dump_dir":/backups:ro "$PG_IMAGE" \
  pg_restore --list "/backups/$dump_file" | awk '$4 == "INDEX" { print $6 }' | sort -u)"
actual="$($COMPOSE exec -T "$SERVICE" psql -U "$PGUSER" -d "$PGDATABASE" -tAc \
  "SELECT indexname FROM pg_indexes WHERE schemaname NOT IN ('pg_catalog','information_schema')" \
  | tr -d '\r' | sed '/^$/d' | sort -u)"
missing="$(comm -23 <(printf '%s\n' "$expected") <(printf '%s\n' "$actual"))"

if [ -n "$missing" ]; then
  echo "!! indexes in the dump that were NOT created:" >&2
  printf '   %s\n' $missing >&2
  echo "   If this is an HNSW build, the usual cause is /dev/shm: set shm_size on" >&2
  echo "   the postgres service, then re-create the index by hand." >&2
  exit 1
fi
echo "indexes: $(printf '%s\n' "$expected" | grep -c .) expected, all present"

if [ "${invalid:-0}" -gt 0 ]; then
  echo "!! ${invalid} index(es) are INVALID — the restore did not fully succeed" >&2
  exit 1
fi

[ "$restore_rc" -eq 0 ] || { echo "!! pg_restore reported errors (exit $restore_rc) — review the log above" >&2; exit 1; }

if [ "$start_worker" -eq 1 ]; then
  echo ">> starting '$WORKER_SERVICE'"
  $COMPOSE up -d "$WORKER_SERVICE"
else
  echo ">> '$WORKER_SERVICE' left stopped. Start it with: $COMPOSE up -d $WORKER_SERVICE"
fi

echo ">> restore complete"
