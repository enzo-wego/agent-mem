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

Measured: 8 streams ≈ 610 KB/s, ~4.5× a single stream. It verifies sha256 on
both ends at the end, and is safe to re-run — ranges are simply rewritten.

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

### Rollback

Until the VPS stack is deleted, rollback is: stop the payments worker, start the
VPS worker. The VPS database is untouched by any of the above — every step
reads from it. Keep the VPS intact for at least a few days after cutover.

## Post-cutover hardening

Both of these are required for a machine expected to run a 24/7 worker, and
neither is on by default:

- **colima must autostart.** `brew services list` shows colima as `none`, so
  after a reboot the VM — and every container — stays down. `brew services
  start colima` installs a LaunchAgent, which runs at *login*; a Mac that
  reboots to a login window will not start it. Enable automatic login, or
  install a LaunchDaemon, if unattended reboots must recover.
- **The Mac must not sleep.** `sudo pmset -a sleep 0 disksleep 0` (needs an
  interactive sudo). `KeepingYouAwake.app` is installed and is a GUI
  alternative.

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
