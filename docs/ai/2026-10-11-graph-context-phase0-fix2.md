# Phase 0a fix round 2

Branch `feat/graph-context-phase0` (HEAD 1a594e7), worktree
`/Users/enzo/go/src/github.com/agent-mem-wt-gc0`. Source: delta deep-review of
78e62ab..1a594e7 (2026-10-11). Same limits as before: scratch DB only, no hub eval,
no merge, no deploy, no container restarts. Two items only; change nothing else.

A. `trace show` says "same as returned" after text was appended.
   `internal/graph/handlers/trace_render.go:35-38` always truncates the current body
   to the recorded `body_bytes`, so an appended tail is cut off and the hash still
   matches. Truncate only when the stored trace item has `body_truncated: true`
   (resolve records it at `resolve.go:497`); otherwise hash the full current body.
   While there, reuse `hydrate`'s truncation helper instead of the copy at
   `trace_render.go:68` (export it if needed, behaviour unchanged). Add a test in
   `cmd/agent-mem/trace_test.go` that appends to `body_full` of an untruncated item
   and expects `differs from returned`; keep the existing prefix-edit and truncated
   cases passing. Check the new test fails on 1a594e7.

B. `make deploy-payments` stamps the wrong BUILD_REV.
   `Makefile:135` evaluates `$$(git rev-parse HEAD)` on the remote, inside the ssh
   string, but rsync excludes `.git/` (`Makefile:129`). Compute the revision locally
   (for example `BUILD_REV=$(shell git rev-parse HEAD)` expanded by make before ssh)
   and pass it into the remote command; refuse to deploy when the local tree is dirty
   or the revision is empty (print why and exit non-zero). Show in the summary the
   exact command line `make -n deploy-payments` prints.

Verification to paste: `make test-db`, `TZ=UTC make test-db`, `go vet ./...`,
`go test ./cmd/...`, `make -n deploy-payments`. One commit per item, push the branch.
