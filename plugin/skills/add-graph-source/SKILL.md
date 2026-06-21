---
name: "add-graph-source"
description: "Add a new external artifact source (Slack/Jira/GitHub/.../Wego Hub style) to Graph Memory. Use when wiring a new system whose documents should be fetched, normalized, embedded, and made searchable in the graph."
---

Add a new data source to Graph Memory end-to-end. A "source" is any external system (Slack, Jira, Confluence, Wego Hub, …) whose artifacts become graph nodes: fetched by URL/ID, normalized to plain text, edge-extracted, embedded, and served via `/search` and `/resolve`.

The pipeline is fixed — `ingest/url` (or backfill) → `fetch_body` (fetch + normalize + edges) → `index_artifact` (embed). You only plug in a **fetcher** (how to retrieve), a **normalizer** (how to turn the raw body into text), an **ID convention**, and the **config/auth** to reach the system. Mirror an existing source: **Confluence** is the cleanest template (fetch a document by ID/URL over REST with a token, return an HTML/XML-ish body that gets stripped to text).

## Decision checklist before coding

1. **Node granularity** — what is one node? (Slack = one thread, Jira = one issue, Confluence = one page, Wego Hub = one slug.) The natural key is the stable ID of that unit.
2. **Node ID shape** — `type:natural_key` (e.g. `wegohub:q4-report`, `cf:12345`). Pick a short `type` prefix.
3. **Auth** — what credential reaches the read API? (Bearer token, basic auth, API+App key.) One env var per secret, `AGENT_MEM_<SOURCE>_TOKEN`.
4. **Scope / ACL** — what `scope` does a node get, and who may see it? Team-private → a scope derived from metadata (e.g. `slack:<channel>`). Internal-public → return `"public"` (the acl builder grants `public` to every scoped asker; see `internal/graph/acl/scopes.go`).
5. **Body format** — JSON? HTML? Markdown? The normalizer turns it into plain UTF-8. HTML/XML → reuse `confluenceFallback` (tag-strip + entity-unescape) in the normalizer package.

## File-by-file wiring

Replace `<src>` with the source name (e.g. `wegohub`) and `<Type>`/`<ctor>` accordingly. ~11 edits + 2 new files.

| # | File | Change |
|---|------|--------|
| 1 | `internal/graph/ids/ids.go` | Add `Type<Src> NodeType = "<src>"` to the const block; add an ID builder `func <Src>(key string) (string, error)` (validate the key with a regex, like `Jira`/`WegoHub`); add `Type<Src>` to the `ParseType` switch. |
| 2 | `internal/graph/fetchers/fetchers.go` | Add token/base-URL fields to `Config`; default the base URL in `NewRegistry`; append `new<Src>Fetcher(cfg, log)` to `r.fetchers`. |
| 3 | `internal/graph/fetchers/<src>.go` | **New.** Implement `Fetcher`: `Source()`, `Matches(nodeIDorURL)` (regex on `<src>:…` and the URL), `Fetch(ctx, node)` → `FetchedBody{NodeID, Type, URL, Title, Raw, ContentType, Author, BodyTS, Metadata}`. |
| 4 | `internal/graph/normalizer/normalizer.go` | Register `New<Src>Normalizer()` in `NewDefault`. |
| 5 | `internal/graph/normalizer/<src>.go` | **New.** Implement `Normalizer`: `Source()` and `Normalize(ctx, raw, meta) → Result{Text}`. For HTML, `return Result{Text: confluenceFallback(raw)}`. |
| 6 | `internal/graph/handlers/url_patterns.go` | Add `<src>URLPattern` regex capturing the natural key from a URL. |
| 7 | `internal/graph/handlers/ingest_url.go` | Add a `case "<src>":` to `nodeIDFromURL` that builds the node ID from the URL match. |
| 8 | `internal/graph/handlers/fetch_body.go` | Add a `case "<src>":` to `deriveScope` returning the node's scope (`"public"` for internal-public sources, else `"<src>:" + key` from metadata). |
| 9 | `internal/graph/handlers/describe_attachment.go` | Add a `case "<src>":` to `downloadWithAuth` setting the auth header for attachment downloads (skip if the source has none). |
| 10 | `internal/config/config.go` | Add the token/base-URL fields to `GraphConfig`; load them from `AGENT_MEM_<SRC>_TOKEN` / `_BASE_URL` env vars in the env-loading block. |
| 11 | `internal/worker/server.go` | Map the new config fields in `fetchersConfigFromAppConfig`. |
| 12 | `README.md` | Add the env var to the Graph Memory config table and the source to the two prose lists. |

**Tests** (mirror `*_test.go` siblings): a fetcher test (`Matches` truth table + a happy path against `httptest.NewServer` + a degraded path), an `ids` builder test (valid/invalid keys + `ParseType`), and — if you touched `deriveScope`/acl — a scope assertion.

## The fetcher contract (most important)

```go
func (f *<src>Fetcher) Source() string { return "<src>" }
func (f *<src>Fetcher) Matches(s string) bool { return nodeRe.MatchString(s) || urlRe.MatchString(s) }
func (f *<src>Fetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
    key, err := f.parseNode(node)          // accept either "<src>:key" or a URL
    // ... GET the artifact with auth from f.cfg ...
    nodeID, _ := ids.<Src>(key)
    return FetchedBody{
        NodeID: nodeID, Type: ids.Type<Src>,
        URL: permalink, Title: title,
        Raw: bodyBytes, ContentType: "text/html",   // normalizer decides what to do
        Author: AuthorRef{Source: "<src>", Email: ownerEmail}, // email resolves to a person
        BodyTS: updatedAt,                           // ingest tiebreaker — never overwrite newer
        Metadata: map[string]any{ /* keys deriveScope reads */ },
    }, nil
}
```

- Return `Raw` untouched; the normalizer owns text extraction.
- `Author.Email` (when known) lets identity resolution dedupe the person and power team-affinity scoring. Use email over an opaque ID when you have it.
- Make `Fetch` resilient: if a metadata/list call fails, degrade to fetching the primary document rather than erroring the whole job.
- Fatal vs transient: a 4xx the source will never satisfy → wrap so the dispatcher stops retrying; a 5xx/timeout → transient (the handler already classifies fetch errors as transient).

## Worked example: Wego Hub (`wegohub`)

Internal file-publishing platform. One node = one slug (`wegohub:<slug>`), served at `internal.wego.com/hub/apps/<slug>`. Files are **internal-public** → scope `"public"`. Metadata (description, owner, file list) comes from `GET /hub/api/files/:slug` behind a Bearer token; the served `index.html` is fetched without auth and stripped to text. See `internal/graph/fetchers/wegohub.go` and `internal/graph/normalizer/wegohub.go` for the full reference implementation, and commit history for the 11-file wiring.

## Verify

```bash
go build ./... && go test ./internal/graph/... ./internal/config/ -count=1
```

Then end-to-end against a running worker (the read endpoints need `AGENT_MEM_API_KEY`):

```bash
# Ingest one artifact by URL
curl -s -H "Authorization: Bearer $AGENT_MEM_API_KEY" -X POST \
  localhost:34567/api/graph/ingest/url -d '{"url":"https://internal.wego.com/hub/apps/<slug>"}'
# Watch it flow: queued fetch_body → index_artifact → embedding populated
# Then it is searchable:
curl -s -H "Authorization: Bearer $AGENT_MEM_API_KEY" \
  "localhost:34567/api/graph/search?q=<something in the doc>&limit=5"
```

A node with no body after `fetch_body` ran usually means the fetcher returned empty `Raw` or the normalizer produced empty text. A node that never appears in `/search` for a scoped asker is an ACL/scope mismatch — check `deriveScope` and that internal-public sources return `"public"`.
