# Brief: persist the hub-topology change into CLAUDE.md and memory

Enzo confirmed 2026-08-15: the agent-mem hub moved PERMANENTLY from the VPS
(`enzo@enzogo.io.vn`, Linux/Ubuntu x86_64 — now retired, worker stopped) to the
**payments Mac mini** (`ssh enzo@payments`, Tailscale `100.125.54.118`, arm64,
repo `~/go/src/github.com/agent-mem`, docker at `/opt/homebrew/bin`). Apply the
three edits below exactly. Text-only round: no Go, no build needed.

## Edit 1 — repo `CLAUDE.md`

Insert a new section between "## Build & Test" (keep it) and
"## Architecture Overview":

```markdown
## Deployment — hub is the payments Mac mini, NOT the VPS

Since 2026-08-12 (confirmed permanent 2026-08-15) the production agent-mem hub
runs on the **payments Mac mini**: `ssh enzo@payments` (Tailscale
`100.125.54.118`, arm64, repo at `~/go/src/github.com/agent-mem`, docker at
`/opt/homebrew/bin`). The old VPS (`enzo@enzogo.io.vn`, Linux/Ubuntu x86_64) is
**retired** — its worker is stopped and port 34567 refuses connections.

- **Do NOT use `make deploy`** until it is fixed: it still targets the dead VPS
  and builds `linux/amd64` only.
- **Deploy** (the Mac builds arm64 natively — no GHCR round trip):

  ```bash
  ssh enzo@payments 'cd ~/go/src/github.com/agent-mem && git pull --ff-only \
    && PATH=/opt/homebrew/bin:$PATH docker compose up -d --build worker'
  ```

- **Verify the running binary carries the change** (never trust the build log):

  ```bash
  ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker exec agent-mem-worker-1 \
    grep -c "<new string literal>" /usr/local/bin/agent-mem'
  ```

- Dashboard changes still require the embed rebuild BEFORE committing:
  `cd dashboard && npm run build`, then from the repo root
  `rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/`.
```

## Edit 2 — memory file
`/Users/neocapitelo/.claude/projects/-Users-neocapitelo-go-src-github-com-agent-mem/memory/project_agent_mem_hub_topology.md`

- Replace the `description:` frontmatter line with:
  `description: "PERMANENT (user-confirmed 2026-08-15): agent-mem hub moved from the VPS enzogo.io.vn (Linux/Ubuntu) to the payments Mac mini (ssh enzo@payments, 100.125.54.118, arm64) — deploy path differs"`
- Prepend to the body's first paragraph (before "On 2026-08-12 ~19:07Z"):
  `Enzo confirmed 2026-08-15 this is the permanent topology, not a temporary handoff: the VPS ('enzogo.io.vn', Linux/Ubuntu) is retired as the hub; the payments Mac mini is the hub. `

## Edit 3 — memory index
`/Users/neocapitelo/.claude/projects/-Users-neocapitelo-go-src-github-com-agent-mem/memory/MEMORY.md`

Replace the existing `project_agent_mem_hub_topology` line with:
`- [project_agent_mem_hub_topology.md](project_agent_mem_hub_topology.md) — PERMANENT: hub = payments Mac mini (ssh enzo@payments), VPS enzogo.io.vn retired; make deploy targets the dead VPS`

## Acceptance

- All three files contain the new text verbatim; nothing else changed.
- Do NOT commit anything — leave CLAUDE.md in the working tree.
