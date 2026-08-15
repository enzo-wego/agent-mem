# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->


## Build & Test

_Add your build and test commands here_

```bash
# Example:
# npm install
# npm test
```

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

## Architecture Overview

_Add a brief overview of your project architecture_

## Conventions & Patterns

_Add your project-specific conventions here_
