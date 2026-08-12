#!/usr/bin/env bash
# Dump the agent-mem Postgres database to a timestamped custom-format archive.
#
# Custom format (-Fc), not plain SQL, for three reasons:
#   - it is compressed on the way out (a ~3 GB database lands around 1.5 GB),
#   - pg_restore can rebuild into a differently-named database without editing,
#   - indexes are stored as CREATE INDEX statements rather than as heap pages,
#     so a restore REBUILDS every HNSW index from scratch. That property is the
#     reason this script is also the fix-of-record for pgvector index
#     corruption: dump, restore, and the corrupt index is gone.
#
# The dump runs inside the postgres container so the host needs no pg_dump, and
# so the client version always matches the server version. Output is streamed to
# the host over stdout — nothing large is ever written into the container layer.
#
# Usage:
#   scripts/db-backup.sh                  # -> ./backups/agentmem-<ts>.dump
#   BACKUP_DIR=/mnt/big scripts/db-backup.sh
#   DOCKER="sudo docker" scripts/db-backup.sh     # Linux hosts with root-owned socket
#   KEEP=5 scripts/db-backup.sh           # prune to the 5 most recent dumps
set -euo pipefail

# On the Linux VPS the docker socket is root-owned and needs sudo; under colima
# on macOS it is owned by the login user and must NOT. Override per host rather
# than guessing.
DOCKER="${DOCKER:-docker}"

# Addressed through compose, not by container name, so this works regardless of
# what the compose project happens to be called on a given host.
COMPOSE="${COMPOSE:-$DOCKER compose}"
SERVICE="${SERVICE:-postgres}"

PGUSER="${PGUSER:-agentmem}"
PGDATABASE="${PGDATABASE:-agentmem}"

# pg_dump's default is zlib, applied per data block, which leaves a lot of
# cross-block redundancy on the table — this data is mostly vectors rendered as
# text, and repeats heavily between rows. Measured on a 200 MB slice of a real
# archive: re-compressing zlib output with zstd still removed 40% (gzip managed
# 7%), so zlib is leaving most of it on the floor. Over a 250 ms link that
# difference is the difference between a 35 and a 20 minute cutover.
#
# pg_restore reads this natively, so nothing downstream needs a decompress step.
# Requires a server built with zstd (PG16's pgvector image is); set
# COMPRESS=zlib:6 for a target that is not.
COMPRESS="${COMPRESS:-zstd:9}"

# Used only to verify the finished archive, in a throwaway container. Keep in
# step with the postgres image in docker-compose.yml.
PG_IMAGE="${PG_IMAGE:-pgvector/pgvector:pg16}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKUP_DIR="${BACKUP_DIR:-$REPO_ROOT/backups}"

# 0 disables pruning. Any positive value keeps that many newest dumps.
KEEP="${KEEP:-0}"

cd "$REPO_ROOT"
mkdir -p "$BACKUP_DIR"

stamp="$(date +%Y%m%d-%H%M%S)"
out="$BACKUP_DIR/agentmem-$stamp.dump"
log="$BACKUP_DIR/agentmem-$stamp.log"

echo ">> dumping $PGDATABASE from service '$SERVICE' -> $out"

# --verbose goes to stderr, which we keep as a sidecar log: on a failed restore
# the first question is always "did the dump actually finish?", and the tail of
# this file answers it.
#
# NOTE the trailing `-T`: without it compose allocates a TTY and mangles the
# binary stream with CRLF translation, producing an archive pg_restore rejects.
if ! $COMPOSE exec -T "$SERVICE" \
      pg_dump -U "$PGUSER" -d "$PGDATABASE" -Fc --compress="$COMPRESS" --verbose > "$out" 2> "$log"; then
  echo "!! pg_dump failed; see $log" >&2
  tail -5 "$log" >&2 || true
  # A partial archive is worse than no archive: it looks restorable and is not.
  rm -f "$out"
  exit 1
fi

# pg_dump can exit 0 having written nothing if the container died mid-stream.
# A valid custom-format archive always starts with the magic bytes "PGDMP".
if [ ! -s "$out" ] || [ "$(head -c 5 "$out")" != "PGDMP" ]; then
  echo "!! $out is not a valid custom-format archive" >&2
  rm -f "$out"
  exit 1
fi

# Prove the archive's table of contents is readable before calling it a backup.
# This catches truncation that the magic-byte check cannot.
#
# The archive is mounted into a throwaway container rather than piped in:
# pg_restore seeks within a custom-format archive, so reading one from a pipe
# fails outright with "did not find magic string in file header" — a false
# corruption report on a perfectly good dump.
if ! $DOCKER run --rm -v "$BACKUP_DIR":/backups:ro "$PG_IMAGE" \
      pg_restore --list "/backups/$(basename "$out")" > /dev/null 2>&1; then
  echo "!! $out has an unreadable table of contents — treating as corrupt" >&2
  exit 1
fi

echo ">> ok: $(du -h "$out" | cut -f1) $out"

if [ "$KEEP" -gt 0 ]; then
  # ls -t orders newest-first; everything past $KEEP is pruned along with its log.
  mapfile -t stale < <(ls -t "$BACKUP_DIR"/agentmem-*.dump 2>/dev/null | tail -n +$((KEEP + 1)))
  for f in "${stale[@]:-}"; do
    [ -n "$f" ] || continue
    echo ">> pruning $f"
    rm -f "$f" "${f%.dump}.log"
  done
fi
