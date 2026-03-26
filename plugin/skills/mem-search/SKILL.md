---
name: "search"
description: "Search past coding sessions and observations from agent-mem memory"
tools: ["Bash"]
---

Search your past coding sessions, observations, and summaries stored in agent-mem.

## How to search

Use curl to query the agent-mem worker API:

```bash
# Hybrid search (FTS + semantic)
curl -s "http://localhost:34567/api/search?q=QUERY&project=PROJECT&limit=10"

# Search by file
curl -s "http://localhost:34567/api/search/by-file?path=FILE_PATH&project=PROJECT"

# Timeline search
curl -s "http://localhost:34567/api/search/timeline?project=PROJECT&from=2026-01-01&to=2026-03-21"

# List observations by type
curl -s "http://localhost:34567/api/observations?project=PROJECT&type=bugfix&limit=20"

# Get observation details
curl -s "http://localhost:34567/api/observations/ID"
```

## Parameters

- `q` — search query (required for /api/search)
- `project` — project name filter
- `limit` — max results (default 10)
- `type` — observation type filter: decision, bugfix, feature, refactor, discovery
- `from`/`to` — date range in YYYY-MM-DD format

## When to use

- When the user asks about past work, previous sessions, or what was done before
- When looking for context about a specific file, feature, or bug
- When the user says "remember when" or "what did we do about"
