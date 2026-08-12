# Migrating agent-mem from the VPS to `payments`

Moving the worker, its Postgres/pgvector database and llm-gateway off the
RackNerd VPS (`enzogo.io.vn`, x86_64 Linux) onto `payments` (Apple M4, macOS,
colima). Written while performing the migration, so the gotchas below are the
ones actually hit rather than the ones anticipated.

## What changes, and why it is not just "run the same compose file"

| | VPS | payments |
|---|---|---|
| Host | RackNerd, x86_64 Linux | Apple M4, macOS 26.6, 16 GB |
| Container runtime | dockerd, root-owned socket (`sudo docker`) | colima 4 CPU / 6 GiB / 60 GiB, user-owned socket (no sudo) |
| Image arch | `linux/amd64` | `linux/arm64` |
| Image source | built on the VPS, pushed to GHCR, pulled back | built in place — an M4 is faster than cross-building under QEMU |
| Init | systemd | launchd; **colima itself does not autostart** |

Three consequences worth stating plainly:

- **`sudo docker` vs `docker`.** Every script here takes `DOCKER` as an
  environment variable rather than guessing. The Makefile defaults it to
  `sudo docker` for the VPS; on payments pass `DOCKER=docker`.
- **The image must be arm64.** The Dockerfile already selects the right
  LiteParse native binary from `uname -m`, so this needs no source change —
  only a build on the target. Verified: `lit --version` → 2.0.0 on arm64.
- **`docker-compose.override.yml` is host-specific and must never be synced.**
  The VPS copy pins the amd64 GHCR image; using it on payments would run (or
  fail to run) the wrong architecture. `make deploy-payments` excludes it.

## Backup

```bash
# On the VPS
make db-backup                       # -> ./backups/agentmem-<ts>.dump
KEEP=5 make db-backup                # keep only the 5 newest
```

`scripts/db-backup.sh` runs `pg_dump -Fc` inside the postgres container and
streams the archive to the host, so the client version always matches the
server and nothing large lands in the container layer. It then verifies the
archive: magic bytes plus a full table-of-contents read.

**Gotcha, learned the hard way:** `pg_restore --list` cannot read a custom-format
archive from a pipe — it seeks, and a pipe gives `did not find magic string in
file header`. Verification therefore mounts the file into a throwaway container.
Piping it produces a false corruption report on a perfectly good dump.

A logical dump is also the repair path for a **corrupt HNSW index**: indexes
travel as `CREATE INDEX` statements, so a restore rebuilds every one of them
from the table data. Corruption cannot survive the round trip.

## Restore

```bash
# On payments
cd /Users/enzo/src/github.com/agent-mem
DOCKER=docker ./scripts/db-restore.sh --force /Users/enzo/backups/agentmem-<ts>.dump
```

The script drops and recreates the database rather than restoring over the top
— a restore into a populated database leaves stale rows wherever the dump has
no matching key, silently producing a hybrid of two snapshots. It stops the
worker first and **leaves it stopped**, so you can inspect the data before
anything writes to it, and before a second worker starts answering the same
Slack events as the one still running elsewhere. Pass `--start-worker` to
override.

After restoring it prints the extension list and top tables, counts HNSW
indexes, and fails if any index is left `indisvalid = false` — `pg_restore`
exits 0 even when individual objects fail, so the exit code alone is not proof.

### The one that actually bit: /dev/shm and the missing vector index

The first full restore looked fine and was not. `pg_restore` reported
`errors ignored on restore: 2`, and buried in the log was:

```
CREATE INDEX idx_observations_embedding ON public.observations
    USING hnsw (embedding public.vector_cosine_ops);
ERROR:  could not resize shared memory segment ... No space left on device
```

Docker gives a container 64 MB of `/dev/shm`; a parallel HNSW build over
`public.observations` (1.18 GB) needs more. The result is the worst kind of
failure — the primary vector index was **absent** from a database that
otherwise restored perfectly, and every memory search would have silently
fallen back to a sequential scan.

Two fixes, both now in the repo:

- `shm_size: 1gb` on the postgres service in `docker-compose.yml`.
- `db-restore.sh` compares the index names in the archive's TOC against
  `pg_indexes` and fails on any that are absent. The earlier `indisvalid`
  check could not catch this: a `CREATE INDEX` that fails outright leaves no
  index behind, so there is nothing for it to mark invalid.

Rebuilding by hand, if it ever happens again (14 s on an M4):

```sql
SET maintenance_work_mem = '1GB';
SET max_parallel_maintenance_workers = 2;
CREATE INDEX idx_observations_embedding ON public.observations
    USING hnsw (embedding vector_cosine_ops);
```

### Verifying a restore against the source

`n_live_tup` is a stale estimate and will not match — compare real counts:

```sql
SELECT string_agg(format('SELECT %L AS t, count(*) AS c FROM %I.%I',
       n.nspname||'.'||c.relname, n.nspname, c.relname), ' UNION ALL ')
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r' AND n.nspname IN ('public','graph');
```

Run the generated query on both hosts and diff. Every table should match
exactly, except those the source worker wrote to after the dump was taken —
and those must be *lower* on the target, never higher.

## Moving the dump between hosts

The two hosts are ~250 ms apart. A single TCP stream over that RTT measured
**~130 KB/s**, which is about three hours for a 1.5 GB dump, regardless of
available bandwidth — the send window drains faster than acknowledgements
return. `scripts/send-dump.sh` splits the file into N byte ranges and writes
each straight into the destination with `dd seek=`, giving N times the in-flight
window and no part files to reassemble:

```bash
./scripts/send-dump.sh backups/agentmem-<ts>.dump enzo@payments \
    /Users/enzo/backups/agent-mem/agentmem-<ts>.dump 8
```

Measured on this pair: 1 stream ≈ 130 KB/s, 8 streams ≈ 760 KB/s, **16 streams
≈ 1.4 MB/s**. Use 16. It verifies sha256 on both ends at the end, and is safe
to re-run — ranges are simply rewritten.

### Compress with zstd, not zlib

`pg_dump -Fc` defaults to zlib applied per data block, which leaves most of the
redundancy in place: this data is largely vectors rendered as text and repeats
heavily between rows, and zlib never sees across block boundaries. Measured on
a 200 MB slice of a real archive:

| | size | vs raw |
|---|---|---|
| raw (zlib archive) | 209.7 MB | — |
| re-compressed with `gzip -6` | 194.1 MB | −7% |
| re-compressed with `zstd -3` | 125.6 MB | **−40%** |

So `db-backup.sh` defaults to `--compress=zstd:9`. On the real database that
turned a 1.5 GB archive into **892 MB — 41% smaller**, cutting the cutover
transfer from ~35 min to ~17. `pg_restore` reads it natively, so there is no
separate decompress step; PG16's pgvector image has zstd built in. Set
`COMPRESS=zlib:6` for a target that does not.

## llm-gateway

The gateway is reached by **container name**, not service name: the worker's
stored `llm_gateway_url` setting is `http://llm-gateway-llm-gateway-1:8750`.
That resolves only because the gateway container joins the `agent-mem_default`
network. So on payments:

1. Bring up agent-mem's postgres first — that creates `agent-mem_default`.
2. Start the gateway with the network overlay:
   ```bash
   cd /Users/enzo/src/github.com/llm-gateway
   docker compose -f docker-compose.yml -f docker-compose.vps.yml up -d
   ```

The overlay file is named `vps` but contains nothing VPS-specific — it attaches
the external `agent-mem_default` network and publishes 8750 on loopback, which
is exactly what payments needs too. Worth renaming to something host-neutral
once the VPS is retired.

**Credentials.** The Claude seat authenticates from `CLAUDE_CODE_OAUTH_TOKEN`
in `config/.env`, not from a credentials file — there is no
`.claude/.credentials.json` in the volume. The `claude-home` volume carries
session and project state only. To move it:

```bash
# VPS
docker run --rm -v llm-gateway_claude-home:/data:ro -w /data alpine \
    tar czf - . > claude-home.tar.gz
# payments
docker volume create llm-gateway_claude-home
docker run --rm -v llm-gateway_claude-home:/data -v /path/to/backups:/b:ro alpine \
    sh -c 'cd /data && tar xzf /b/claude-home.tar.gz'
```

Smoke test after starting, which exercises both backends:

```bash
KEY=$(grep '^LLM_GATEWAY_API_KEY=' config/.env | cut -d= -f2-)
curl -s http://127.0.0.1:8750/health
curl -s -X POST http://127.0.0.1:8750/generate -H "X-API-Key: $KEY" \
     -H 'Content-Type: application/json' \
     -d '{"system":"Reply with one word.","user":"Say: migrated","tier":"cheap"}'
curl -s -X POST http://127.0.0.1:8750/embed -H "X-API-Key: $KEY" \
     -H 'Content-Type: application/json' \
     -d '{"texts":["smoke test"],"dims":768}'
```

## Cutover

The hazard is **two workers holding the same Slack, Jira and GitHub tokens**.
Both would process the same events and send duplicate DMs, and both would spend
against the same Claude seat. Only one worker may run at a time.

Order matters:

1. **Stop the VPS worker first.** `ssh enzo@enzogo.io.vn 'cd … && sudo docker compose stop worker'`
   From this moment nothing writes to the VPS database, which is what makes the
   dump taken next authoritative.
2. Take the final dump on the VPS: `make db-backup`.
3. Ship it: `scripts/send-dump.sh …` (allow ~50 min for 1.5 GB).
4. Restore on payments: `DOCKER=docker ./scripts/db-restore.sh --force …`.
5. Verify row counts against the VPS numbers recorded before the cutover.
6. Start the payments worker: `docker compose up -d worker`.
7. Stop the VPS gateway last: `sudo docker compose -f … stop llm-gateway` — it
   is the fallback if step 6 fails.

Downtime is dominated by the transfer, not the restore. Do it in a quiet window.

### What the cutover actually cost (2026-08-12)

| | |
|---|---|
| 18:28 | VPS worker stopped — database frozen, dump now authoritative |
| 18:28–18:37 | fresh `zstd:9` dump, 892 MB |
| 18:41–18:58 | transfer, 16 streams, sha256 verified |
| 19:02–19:03 | restore, 82 s, all 60 indexes present first time |
| 19:04 | worker started — **and immediately failed every outbound call** |
| 19:07 | proxy removed from `.env`, worker restarted clean |

**39 minutes, zero rows lost** — all 35 tables matched the VPS exactly at
2,388,185 rows.

### The `.env` proxy trap

The worker came up and every Slack, Jira and GitHub call failed with:

```
proxyconnect tcp: dial tcp 172.18.0.1:8888: connect: connection refused
```

`.env` carries `HTTP_PROXY`/`HTTPS_PROXY=http://172.18.0.1:8888`, an SSH tunnel
that exists only on the VPS — `172.18.0.1` is that host's docker bridge. Copying
`.env` verbatim carries the setting to a machine where nothing is listening.

payments reaches all four upstreams directly (Slack, Jira, GitHub, OpenRouter
all verified from inside a container on `agent-mem_default`), so the fix is to
comment the proxy lines out. Note this is **not** a restart-in-place fix:
compose snapshots `env_file` at container *create* time, so it needs
`docker compose up -d --force-recreate worker`.

The failures were all classed transient and retried, so nothing was lost — but
check `graph.jobs` for `status='failed'` after any such incident before
assuming that.

### Rollback

Until the VPS stack is deleted, rollback is: stop the payments worker, start the
VPS worker. The VPS database is untouched by any of the above — every step
reads from it. Keep the VPS intact for at least a few days after cutover.

## Post-cutover hardening

Both of these are required for a machine expected to run a 24/7 worker, and
neither is on by default:

- **colima must autostart — and `brew services` is not enough here.**
  `brew services list` shows colima as `none`, so after a reboot the VM and
  every container stay down. The obvious fix, `brew services start colima`,
  installs a *LaunchAgent*, and a LaunchAgent only runs once its user logs in.
  On this machine that never happens for the right user:

  | | |
  |---|---|
  | owns the colima VM (`/Users/enzo/.colima`) | `enzo` |
  | GUI console user | `mysqto` |
  | auto-login user | `payments` |

  `enzo` gets no login session at boot, so the agent would never fire. Use a
  **LaunchDaemon** instead — it runs at boot with no login, and `UserName`
  pins it to `enzo`. The plist is staged at `/Users/enzo/io.colima.enzo.plist`:

  ```bash
  sudo cp /Users/enzo/io.colima.enzo.plist /Library/LaunchDaemons/
  sudo chown root:wheel /Library/LaunchDaemons/io.colima.enzo.plist
  sudo chmod 644        /Library/LaunchDaemons/io.colima.enzo.plist
  sudo launchctl bootstrap system /Library/LaunchDaemons/io.colima.enzo.plist
  ```

  Both containers already carry `restart: unless-stopped`, so they return by
  themselves once the VM is up. **This needs a real reboot to prove** — running
  a VM from a LaunchDaemon is the one part of this migration not yet tested.

- **The Mac must not sleep.** Current state is not durable: `sleep` is 0 only
  because of a transient `caffeinate -i -w <pid>`, and `disksleep 10` /
  `autorestart 0` are still set. Needs an interactive sudo:

  ```bash
  sudo pmset -a sleep 0 disksleep 0 autorestart 1 womp 1
  ```

  `autorestart 1` brings the machine back after a power cut; `womp 1` (already
  set) wakes it on network access.

Also worth doing once settled:

- Retire `make deploy` and the GHCR amd64 image, or make it multi-arch.
- Rename `docker-compose.vps.yml` in the llm-gateway repo.
- Schedule `make db-backup` (`KEEP=7`) — there is currently no automated backup.

## Environment notes

- pgvector is **0.8.6** on payments vs **0.8.2** on the VPS. Newer and
  backward-compatible; the restore rebuilds indexes with the new version.
- Postgres is published on `127.0.0.1:5433` on payments, not `0.0.0.0` as on the
  VPS. On a tailnet-joined laptop `0.0.0.0` exposes the database to every device
  on the tailnet; nothing needs that, since the worker connects over the compose
  network as `postgres:5432`.
