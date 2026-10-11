# Code review round 2: graph context Phase 1b masking

Branch `feat/graph-context-phase1b-masking`, HEAD `457cca4`, delta `3afda7a..457cca4` (round 1 reviewed `3afda7a`). deep-review Round 2, read-only.

## Verdict

**REQUEST_CHANGES (narrowed).** F1 is fixed for every round-1 input and for `] > | " ' , ; `` ` after a URL. It is still open for `)`, for `. : ! ( +` directly after a URL, and for non-ASCII whitespace or punctuation after a URL. The Markdown link shape `[text](url)+phone` and an NBSP after a URL both still leak. Unicode digits, Luhn on non-ASCII digits and `%40` emails work. No regressions in protected identifiers. The admin path is untouched: the delta does not change any `internal/graph` file.

## F1 HIGH (Verified), still open: the URL span still swallows PII after `)` or a non-ASCII separator

`internal/redact/redact.go:38` (`https?://[^\s"'`<>\[\]{}|,;]+`), with the trim at `:114-122`.

The exclusion class does not contain `(` or `)`, and the trim only drops trailing `.`, `)` and `:`. RE2 `\s` is ASCII-only, so U+00A0 and similar characters are part of the URL. Output of the shipped `Mask`:

| Input (no space after the URL) | Output |
|---|---|
| `[text](https://jira.x/PAY-1)+6591234567` | unchanged |
| `(https://jira.x/PAY-1)4111111111111111` | unchanged |
| `https://jira.x/PAY-1).+6591234567`, `…):4111…` | unchanged |
| `https://jira.x/PAY-1` + `.`/`:`/`!`/`(`/`+` + phone or PAN | unchanged |
| `https://jira.x/PAY-1` + U+00A0, U+3000, U+2009 or U+200B + phone or PAN | unchanged |
| `https://jira.x/PAY-1` + `،` (Arabic comma) or `，` + phone or PAN | unchanged |

An NBSP after a link is common in Confluence, Google Docs and HTML-converted Jira bodies, and an Arabic comma is common in MEA text. Masked values go into agent and LLM context, and that cannot be undone. The likelihood is lower than in round 1, but the leak is irreversible, so the finding stays HIGH.

**Fix:** use a positive ASCII URL class instead of an exclusion list, for example `https?://[A-Za-z0-9\-._~:/?#@!$&*+=%]+`, and keep trimming trailing `.`, `:` and `!`. `(`, `)`, `'`, `,`, `;` and every non-ASCII character then end the span. The only cost is that a parenthesised URL tail loses protection, which can over-mask but never leak. Add the rows above as must-mask tests. `url.+65…` stays a residual case because `.` and `+` are legal URL characters. I checked this fix in a scratch copy: every row above is masked, `wa.me`, permalinks and the `%40` cases are unchanged, and the shipped `redact_test.go` passes.

## Verified closed or working

- Round-1 rows, plus `] > | " ' , ; `` ` after a URL, and space-less JSON are all masked.
- Unicode digits mask as expected: Arabic-Indic, Extended Arabic-Indic, Devanagari, Thai, full-width and mathematical bold. Mixed-script runs mask too (`+966 ٥٠١ 234 567`, `4111١١١١…`). Luhn runs on the mapped values (`digitsOf`, `:358`). An unlabelled non-Luhn Arabic run stays readable, and a labelled one is masked.
- `%40` emails are masked. Real URLs with `%40` that do not form an email shape stay readable (`?q=a%40b`, `files%40v2/x.png`). Double-encoded redirects mask the whole local part, which is acceptable over-masking.
- These still survive: payment and action refs, `F.4111…`, `booking ref:`, `order=`, Slack ts, permalinks, UUIDs, node ids (`slack_thread:…|+65…` masks only the phone), `wa.me/659…`.
- `go vet` is clean. `go test` passes for `./internal/redact`, `./internal/graph/ids` and `./internal/graph/handlers`. `make test-db` exits 2, but only on `jobs.TestPeriodicTickerReclaimInterleavings` (`no original claim`). That package is untouched by this branch and the failure looks clock-dependent, so it is not attributable to this delta.

## Residual risk (not blocking)

- Phone separators are ASCII only, so `+96650 1234567` and `+966 501 234567` pass unchanged. Consider adding U+00A0, U+2009 and U+202F to `phoneSeps`.
- Performance: an adversarial 1.2 MB string of `"1 "` went from 1.44 s to 1.80 s. The Arabic equivalent now takes 3.1 s, up from about 0 s because those digits were not candidates before. Ordinary text stays at about 0.3 s per MB. This is fine at normal body sizes.
- `Mask:92` builds a closure per `+`. It is harmless, but `digitAt(s, i+1)` would be simpler.

## Receipts

- graph: degraded: code-review-graph not queried; rg confirms that every changed helper is private to `internal/redact`, and the callers of `Mask` are unchanged.
- fan_out: inline: Round-N delta
- Probes: a scratch module that copies the shipped `redact.go` and `ids.go` verbatim, with old-vs-new timing.
- coverage-only validator: `OK: coverage valid`
