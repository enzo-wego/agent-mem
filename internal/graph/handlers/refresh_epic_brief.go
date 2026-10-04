package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/llmjson"
)

// Settings behind the per-epic brief job (both dashboard-editable, Settings →
// Epic briefs). The job is off by default: it is canaried with dry_run jobs
// before the lead flips enabled.
const (
	epicBriefsEnabledKey     = "graph.epic_briefs.enabled"
	epicBriefsMinIntervalKey = "graph.epic_briefs.min_interval_minutes"
	epicBriefsMinIntervalDef = 60
)

// epicBriefSigVersion prefixes every member signature; bump it to force one
// regeneration of every brief (prompt or input-selection change).
var epicBriefSigVersion = "v1"

// epicBriefMaxMembers caps how many member summaries one prompt carries
// (newest first). A first build of a 200-thread epic reads 40 summaries, not
// 200; later runs are deltas.
const epicBriefMaxMembers = 40

// refreshEpicBriefPayload is the JSON payload of a refresh_epic_brief job.
// DryRun builds the prompt, calls the LLM and logs the parsed result without
// touching graph.epic_briefs — the canary path (5 epics, expect 1 call each).
type refreshEpicBriefPayload struct {
	EpicKey string `json:"epic_key"`
	DryRun  bool   `json:"dry_run,omitempty"`
}

// NewRefreshEpicBriefHandler returns the job entry for "refresh_epic_brief".
// The job never reschedules itself: refresh_jira_board enqueues one per
// on-board epic whose member signature changed (enqueueEpicBriefs).
func NewRefreshEpicBriefHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  refreshEpicBriefHandler(deps),
		Systems:  []string{"gemini"},
		PoolSize: 1,
		Lease:    SummaryLease,
		UsesLLM:  true,
	}
}

// briefMember is one epic member as the brief job sees it: a Slack thread
// root, issue, PR or doc with the freshest summary the graph holds for it.
type briefMember struct {
	NodeID  string
	Type    string
	Title   string
	Status  string
	Summary string
	// Version is the member's summary version: thread_summaries.signature for
	// a Slack root, artifact_index.refreshed_at (unix) otherwise, "" when the
	// member has no summary yet. It feeds the member signature so a
	// re-summarized thread changes the brief.
	Version string
	// SummaryAt is when the summary last changed (delta selection compares it
	// with the brief's updated_at).
	SummaryAt time.Time
	LastAt    time.Time
}

// epicBriefSignature is the idempotency key of a brief: sha256 over the
// sorted member ids, each paired with its summary version. Order-independent;
// any member joining, leaving or being re-summarized changes it.
func epicBriefSignature(members []briefMember) string {
	lines := make([]string, 0, len(members))
	for _, m := range members {
		lines = append(lines, m.NodeID+"|"+m.Version)
	}
	sort.Strings(lines)
	h := sha256.New()
	h.Write([]byte(epicBriefSigVersion + "\n"))
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return epicBriefSigVersion + ":" + hex.EncodeToString(h.Sum(nil))
}

// loadBriefMembers returns the epic's members (Slack replies folded into their
// root; the epic node itself excluded) with their current summaries, newest
// activity first.
func loadBriefMembers(ctx context.Context, db *pgxpool.Pool, epicKey string) ([]briefMember, error) {
	rows, err := db.Query(ctx, `
SELECT n.id, n.type, COALESCE(n.title,''), COALESCE(n.metadata->>'status',''),
       COALESCE(NULLIF(ts.overview,''), ts.summary, ai.summary, ''),
       COALESCE(ts.signature, CASE WHEN ai.refreshed_at IS NULL THEN '' ELSE (EXTRACT(EPOCH FROM ai.refreshed_at))::bigint::text END, ''),
       COALESCE(ts.updated_at, ai.refreshed_at, to_timestamp(0)),
       COALESCE(m.last_at, n.created_at, n.first_seen_at)
FROM graph.epic_membership m
JOIN graph.nodes n ON n.id = m.node_id AND n.deleted_at IS NULL
LEFT JOIN graph.thread_summaries ts
       ON n.type IN ('slack','slack_thread')
      AND ts.channel_id = split_part(n.id,':',2) AND ts.thread_ts = split_part(n.id,':',3)
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id AND n.type NOT IN ('slack','slack_thread')
WHERE m.epic_key = $1
  AND m.node_id <> 'jira:' || $1
  AND (n.type NOT IN ('slack','slack_thread')
       OR COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = split_part(n.id,':',3))
ORDER BY COALESCE(m.last_at, n.created_at, n.first_seen_at) DESC NULLS LAST, n.id`, epicKey)
	if err != nil {
		return nil, fmt.Errorf("load epic members: %w", err)
	}
	defer rows.Close()
	var out []briefMember
	for rows.Next() {
		var m briefMember
		var summaryAt, lastAt *time.Time
		if err := rows.Scan(&m.NodeID, &m.Type, &m.Title, &m.Status, &m.Summary, &m.Version, &summaryAt, &lastAt); err != nil {
			return nil, err
		}
		if summaryAt != nil {
			m.SummaryAt = *summaryAt
		}
		if lastAt != nil {
			m.LastAt = *lastAt
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// epicBriefFedValue is what `fed` stores for a member: the signature version
// plus exactly the per-member string epicBriefSignature hashes.
func epicBriefFedValue(m briefMember) string {
	return epicBriefSigVersion + "|" + m.NodeID + "|" + m.Version
}

// pendingMembers returns, in the given (newest-first) order, the members not
// yet fed at their current version: absent from fed, or fed at another value.
// A nil fed makes every member pending. The business root never appears here:
// loadBriefMembers reads only rows keyed by this epic.
func pendingMembers(members []briefMember, fed map[string]string) []briefMember {
	var out []briefMember
	for _, m := range members {
		if v, ok := fed[m.NodeID]; !ok || v != epicBriefFedValue(m) {
			out = append(out, m)
		}
	}
	return out
}

// prunedFed returns a copy of fed restricted to current members.
func prunedFed(fed map[string]string, members []briefMember) map[string]string {
	cur := make(map[string]bool, len(members))
	for _, m := range members {
		cur[m.NodeID] = true
	}
	out := make(map[string]string, len(fed))
	for id, v := range fed {
		if cur[id] {
			out[id] = v
		}
	}
	return out
}

// epicBriefFullBuild reports whether the next build starts from scratch: no
// non-empty stored brief, built under another signature version, or fed
// missing / carrying another version.
func epicBriefFullBuild(existing epicBriefRow, hasExisting bool) bool {
	if !hasExisting || existing.Brief == "" || existing.BuiltVersion != epicBriefSigVersion || existing.Fed == nil {
		return true
	}
	prefix := epicBriefSigVersion + "|"
	for _, v := range existing.Fed {
		if !strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// selectBriefInputs picks the members one prompt carries, capped at
// epicBriefMaxMembers, newest first: the newest members for a full build, the
// newest pending members for a delta. nil means a delta with nothing to feed:
// the caller then refreshes bookkeeping without an LLM call.
func selectBriefInputs(members, pending []briefMember, full bool) []briefMember {
	src := pending
	if full {
		src = members
	}
	if len(src) > epicBriefMaxMembers {
		return src[:epicBriefMaxMembers]
	}
	return src
}

// epicBriefsEnabled reads graph.epic_briefs.enabled.
func epicBriefsEnabled(ctx context.Context, db *pgxpool.Pool) bool {
	v, _ := strconv.ParseBool(strings.TrimSpace(loadSetting(ctx, db, epicBriefsEnabledKey)))
	return v
}

// epicBriefsMinInterval reads graph.epic_briefs.min_interval_minutes (default
// 60; non-positive or unparsable values fall back to the default).
func epicBriefsMinInterval(ctx context.Context, db *pgxpool.Pool) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(loadSetting(ctx, db, epicBriefsMinIntervalKey)))
	if err != nil || n <= 0 {
		n = epicBriefsMinIntervalDef
	}
	return time.Duration(n) * time.Minute
}

// epicBriefRow is the stored brief the job compares against and the read side
// serves.
type epicBriefRow struct {
	EpicKey    string
	Brief      string
	Highlights json.RawMessage
	OpenItems  json.RawMessage
	Signature  string
	Sources    []string
	UpdatedAt  time.Time
	Previous   string
	PreviousAt *time.Time
	// Fed is member node id -> epicBriefFedValue at the last feed; nil when
	// the column is NULL (legacy row). BuiltVersion is "" when NULL.
	Fed          map[string]string
	BuiltVersion string
}

// loadEpicBrief returns the stored brief for epicKey, or ok=false when none.
func loadEpicBrief(ctx context.Context, db *pgxpool.Pool, epicKey string) (epicBriefRow, bool) {
	var r epicBriefRow
	var prev, built *string
	var fed []byte
	err := db.QueryRow(ctx, `
SELECT epic_key, brief, highlights, open_items, member_signature, sources, updated_at, previous, previous_at, fed, built_version
FROM graph.epic_briefs WHERE epic_key = $1`, epicKey).Scan(
		&r.EpicKey, &r.Brief, &r.Highlights, &r.OpenItems, &r.Signature, &r.Sources, &r.UpdatedAt, &prev, &r.PreviousAt, &fed, &built)
	if err != nil {
		return epicBriefRow{}, false
	}
	if prev != nil {
		r.Previous = *prev
	}
	if built != nil {
		r.BuiltVersion = *built
	}
	if fed != nil {
		r.Fed = map[string]string{}
		_ = json.Unmarshal(fed, &r.Fed)
	}
	return r, true
}

// briefItem is one highlight or open item: a short line citing the member
// node ids it rests on.
type briefItem struct {
	Text    string   `json:"text"`
	Sources []string `json:"sources"`
}

// epicBriefOutput is the LLM's parsed answer.
type epicBriefOutput struct {
	Brief      string      `json:"brief"`
	Highlights []briefItem `json:"highlights"`
	OpenItems  []briefItem `json:"open_items"`
}

// buildEpicBriefPrompt renders the user message: epic title + Jira description,
// the previous brief (delta mode), and one block per input member.
func buildEpicBriefPrompt(epicKey, title, description, previous string, inputs []briefMember, delta bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Epic %s: %s\n", epicKey, strings.TrimSpace(title))
	if d := strings.TrimSpace(description); d != "" {
		b.WriteString("Jira description: " + flattenLines(d, 1500) + "\n")
	}
	if delta && previous != "" {
		b.WriteString("\nPrevious brief (update it with the new material below; keep what is still true):\n" + previous + "\n")
		b.WriteString("\nMembers not yet reflected at their current version:\n")
	} else {
		b.WriteString("\nMembers (newest first):\n")
	}
	for _, m := range inputs {
		fmt.Fprintf(&b, "\n[%s] %s", m.NodeID, m.Type)
		if m.Status != "" {
			fmt.Fprintf(&b, " · status %s", m.Status)
		}
		if !m.LastAt.IsZero() {
			fmt.Fprintf(&b, " · %s", m.LastAt.Format("2006-01-02"))
		}
		if t := strings.TrimSpace(m.Title); t != "" {
			b.WriteString("\n  title: " + flattenLines(t, 200))
		}
		if s := strings.TrimSpace(m.Summary); s != "" {
			b.WriteString("\n  summary: " + flattenLines(s, 600))
		}
		b.WriteString("\n")
	}
	return b.String()
}

const epicBriefSystemPrompt = `You maintain a standing brief for one Jira epic of a payments team.
You are given the epic, optionally the previous brief, and a list of member
artifacts (Slack threads, issues, pull requests, docs), each tagged with a node
id in square brackets and a short summary.
Respond as JSON only:
{"brief":"at most 200 words: what the epic is, where it stands, what changed recently",
 "highlights":[{"text":"one short line","sources":["node id","node id"]}],
 "open_items":[{"text":"an unresolved question, blocker or pending task","sources":["node id"]}]}

Rules:
- Every highlight and open item MUST cite at least one node id that appears in
  the input, copied verbatim. Never invent ids, people, dates or outcomes.
- Open items are things NOT done: a member whose status is Done/Closed/Resolved
  is not an open item. Prefer the newest evidence.
- When a previous brief is given, carry forward what is still true and fold in
  the new material; do not restate everything from scratch.
- At most 8 highlights and 8 open items. Keep concrete identifiers (ticket
  keys, payment ids, error codes) verbatim.
No markdown, no prose outside the JSON.`

// genEpicBrief calls the gateway (summary tier) once and parses the answer.
// Item sources are filtered to ids that were actually in the prompt, so a
// hallucinated citation never reaches the table. ok=false on LLM failure or
// an unparsable reply.
func genEpicBrief(ctx context.Context, g GeminiClient, userMsg string, inputs []briefMember) (epicBriefOutput, bool) {
	out, err := g.Generate(ctx, epicBriefSystemPrompt, userMsg)
	if err != nil || strings.TrimSpace(out) == "" {
		return epicBriefOutput{}, false
	}
	var parsed epicBriefOutput
	if json.Unmarshal(llmjson.ExtractJSON(out), &parsed) != nil || strings.TrimSpace(parsed.Brief) == "" {
		return epicBriefOutput{}, false
	}
	known := make(map[string]bool, len(inputs))
	for _, m := range inputs {
		known[m.NodeID] = true
	}
	clean := func(items []briefItem) []briefItem {
		out := make([]briefItem, 0, len(items))
		for _, it := range items {
			it.Text = strings.TrimSpace(it.Text)
			if it.Text == "" {
				continue
			}
			var src []string
			for _, s := range it.Sources {
				if known[s] {
					src = append(src, s)
				}
			}
			if src == nil {
				src = []string{}
			}
			it.Sources = src
			out = append(out, it)
		}
		return out
	}
	parsed.Brief = strings.TrimSpace(parsed.Brief)
	parsed.Highlights = clean(parsed.Highlights)
	parsed.OpenItems = clean(parsed.OpenItems)
	return parsed, true
}

func refreshEpicBriefHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p refreshEpicBriefPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: refresh_epic_brief unmarshal: %v", jobs.ErrFatal, err)
		}
		p.EpicKey = strings.ToUpper(strings.TrimSpace(p.EpicKey))
		if p.EpicKey == "" {
			return fmt.Errorf("%w: refresh_epic_brief: epic_key required", jobs.ErrFatal)
		}
		if deps.Gemini == nil {
			// Misconfiguration, not a data condition: marking the job done
			// would hide the outage (same pattern as notify_watch_channels).
			return fmt.Errorf("%w: refresh_epic_brief: no LLM client configured", jobs.ErrFatal)
		}
		log := deps.Logger.With().Str("epic", p.EpicKey).Bool("dry_run", p.DryRun).Logger()
		// A dry run is the canary: it works even while the feature is off.
		if !p.DryRun && !epicBriefsEnabled(ctx, deps.DB) {
			log.Info().Msg("refresh_epic_brief: disabled (graph.epic_briefs.enabled); skipping")
			return nil
		}

		members, err := loadBriefMembers(ctx, deps.DB, p.EpicKey)
		if err != nil {
			return err
		}
		existing, hasExisting := loadEpicBrief(ctx, deps.DB, p.EpicKey)
		if len(members) == 0 {
			if !hasExisting {
				return nil // no membership rows: unknown epic or rebuild not run yet
			}
			// The epic lost every member: clear the bookkeeping so a later
			// rejoin is fed again. brief and sources stay as the last build
			// left them; no LLM call.
			if p.DryRun {
				log.Info().Msg("refresh_epic_brief: dry run, epic has no members; nothing to do")
				return nil
			}
			_, err := deps.DB.Exec(ctx, `UPDATE graph.epic_briefs SET fed='{}'::jsonb, member_signature=$2 WHERE epic_key=$1`,
				p.EpicKey, epicBriefSignature(nil))
			return err
		}
		sig := epicBriefSignature(members)
		pending := pendingMembers(members, existing.Fed)
		if hasExisting && !p.DryRun {
			if existing.Signature == sig && len(pending) == 0 {
				return nil // idempotent: same members, same summaries, nothing left to feed
			}
			if wait := epicBriefsMinInterval(ctx, deps.DB); time.Since(existing.UpdatedAt) < wait {
				log.Info().Dur("min_interval", wait).Time("updated_at", existing.UpdatedAt).
					Msg("refresh_epic_brief: under the interval floor; next refresh_jira_board re-enqueues")
				return nil
			}
		}
		full := epicBriefFullBuild(existing, hasExisting)
		delta := !full
		inputs := selectBriefInputs(members, pending, full)
		if len(inputs) == 0 {
			// Nothing pending (members left, or were re-summarized to a state
			// already fed). Record the signature and prune fed of departed
			// members in one UPDATE; sources stay (the brief may still
			// reflect them).
			if !p.DryRun {
				fedJSON, _ := json.Marshal(prunedFed(existing.Fed, members))
				_, err := deps.DB.Exec(ctx, `UPDATE graph.epic_briefs SET member_signature=$2, fed=$3 WHERE epic_key=$1`,
					p.EpicKey, sig, fedJSON)
				return err
			}
			log.Info().Msg("refresh_epic_brief: dry run, delta empty; no LLM call")
			return nil
		}

		var title, description string
		_ = deps.DB.QueryRow(ctx, `SELECT COALESCE(title,''), COALESCE(body,'') FROM graph.nodes WHERE id = $1`,
			"jira:"+p.EpicKey).Scan(&title, &description)
		if title == "" {
			_ = deps.DB.QueryRow(ctx, `SELECT epic_summary FROM graph.jira_epic_map WHERE epic_key=$1 AND epic_summary<>'' LIMIT 1`,
				p.EpicKey).Scan(&title)
		}
		previous := ""
		if delta {
			previous = existing.Brief
		}
		userMsg := buildEpicBriefPrompt(p.EpicKey, title, description, previous, inputs, delta)
		out, ok := genEpicBrief(ctx, deps.Gemini, userMsg, inputs)
		if !ok {
			return fmt.Errorf("refresh_epic_brief %s: LLM returned no usable brief", p.EpicKey)
		}
		// sources = every member the current brief rests on: the previous
		// build's list plus this run's inputs (delta), or just the inputs
		// (full build).
		sources := map[string]bool{}
		if delta {
			for _, s := range existing.Sources {
				sources[s] = true
			}
		}
		for _, m := range inputs {
			sources[m.NodeID] = true
		}
		srcList := make([]string, 0, len(sources))
		for s := range sources {
			srcList = append(srcList, s)
		}
		sort.Strings(srcList)
		hl, _ := json.Marshal(out.Highlights)
		oi, _ := json.Marshal(out.OpenItems)

		if p.DryRun {
			log.Info().Int("members", len(members)).Int("inputs", len(inputs)).Bool("delta", delta).
				Str("signature", sig).Str("brief", out.Brief).
				RawJSON("highlights", hl).RawJSON("open_items", oi).
				Msg("refresh_epic_brief: dry run (1 LLM call); not written")
			return nil
		}
		// fed: delta merges this run's inputs into the surviving entries;
		// a full build replaces it. Departed members are dropped either way.
		newFed := map[string]string{}
		if delta {
			newFed = prunedFed(existing.Fed, members)
		}
		for _, m := range inputs {
			newFed[m.NodeID] = epicBriefFedValue(m)
		}
		fedJSON, _ := json.Marshal(newFed)
		var builtVersion *string // delta builds never change built_version
		if !delta {
			v := epicBriefSigVersion
			builtVersion = &v
		}
		_, err = deps.DB.Exec(ctx, `
INSERT INTO graph.epic_briefs (epic_key, brief, highlights, open_items, member_signature, sources, updated_at, previous, previous_at, fed, built_version)
VALUES ($1,$2,$3,$4,$5,$6,NOW(),NULL,NULL,$7,$8)
ON CONFLICT (epic_key) DO UPDATE SET
  previous         = NULLIF(graph.epic_briefs.brief,''),
  previous_at      = CASE WHEN graph.epic_briefs.brief = '' THEN NULL ELSE graph.epic_briefs.updated_at END,
  brief            = EXCLUDED.brief,
  highlights       = EXCLUDED.highlights,
  open_items       = EXCLUDED.open_items,
  member_signature = EXCLUDED.member_signature,
  sources          = EXCLUDED.sources,
  fed              = EXCLUDED.fed,
  built_version    = COALESCE(EXCLUDED.built_version, graph.epic_briefs.built_version),
  updated_at       = NOW()`,
			p.EpicKey, out.Brief, hl, oi, sig, srcList, fedJSON, builtVersion)
		if err != nil {
			return fmt.Errorf("upsert epic brief: %w", err)
		}
		log.Info().Int("members", len(members)).Int("inputs", len(inputs)).Bool("delta", delta).
			Msg("refresh_epic_brief: brief written")
		return nil
	}
}

// enqueueEpicBriefs is called by refresh_jira_board after the membership
// rebuild: one refresh_epic_brief job per on-board epic of the project whose
// member signature differs from the stored brief's, skipping epics that
// already have a queued/running job. No-op while the feature is disabled.
// Returns how many jobs were enqueued.
func enqueueEpicBriefs(ctx context.Context, deps Deps, project string) int {
	if !epicBriefsEnabled(ctx, deps.DB) {
		return 0
	}
	if deps.Gemini == nil {
		deps.Logger.Warn().Msg("enqueue epic briefs: no LLM client configured; enqueueing nothing")
		return 0
	}
	// Candidates: on-board epics plus every epic that already has a brief row,
	// so an epic that left the board or lost all its issues still gets its
	// cleanup run. LIKE $1 keeps the business root ('business:...') out.
	rows, err := deps.DB.Query(ctx, `
SELECT c.k FROM (
  SELECT em.epic_key AS k FROM graph.jira_epic_map em
  WHERE em.on_board AND em.epic_key <> '' AND em.epic_key LIKE $1
  UNION
  SELECT eb.epic_key FROM graph.epic_briefs eb WHERE eb.epic_key LIKE $1
) c
WHERE NOT EXISTS (
    SELECT 1 FROM graph.jobs j
    WHERE j.type = 'refresh_epic_brief' AND j.status IN ('queued','running')
      AND j.payload->>'epic_key' = c.k)`, project+"-%")
	if err != nil {
		deps.Logger.Warn().Err(err).Msg("enqueue epic briefs: list epics failed")
		return 0
	}
	var epics []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil && k != "" {
			epics = append(epics, k)
		}
	}
	rows.Close()

	n := 0
	for _, k := range epics {
		members, err := loadBriefMembers(ctx, deps.DB, k)
		if err != nil {
			continue
		}
		existing, ok := loadEpicBrief(ctx, deps.DB, k)
		if len(members) == 0 {
			// No members: only an epic with a brief row needs work, and only
			// to clear stale fed/signature bookkeeping.
			if !ok || (len(existing.Fed) == 0 && existing.Signature == epicBriefSignature(nil)) {
				continue
			}
		} else if ok && existing.Signature == epicBriefSignature(members) && len(pendingMembers(members, existing.Fed)) == 0 {
			continue
		}
		if _, err := jobs.Enqueue(ctx, deps.DB, "refresh_epic_brief", refreshEpicBriefPayload{EpicKey: k},
			jobs.EnqueueOptions{Priority: 7, TargetRunner: deps.Runner, MachineID: deps.MachineID}); err != nil {
			deps.Logger.Warn().Err(err).Str("epic", k).Msg("enqueue epic briefs: enqueue failed")
			continue
		}
		n++
	}
	return n
}

// epicBriefsConfig is the Settings → Epic briefs payload.
type epicBriefsConfig struct {
	Enabled            bool `json:"enabled"`
	MinIntervalMinutes int  `json:"min_interval_minutes"`
}

// getEpicBriefsConfig serves GET /api/graph/epic-briefs.
func (h *Channels) getEpicBriefsConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	writeJSON(w, http.StatusOK, epicBriefsConfig{
		Enabled:            epicBriefsEnabled(ctx, h.db),
		MinIntervalMinutes: int(epicBriefsMinInterval(ctx, h.db) / time.Minute),
	})
}

// putEpicBriefsConfig serves PUT /api/graph/epic-briefs. Enabled gates the
// next refresh_jira_board enqueue and every queued job; the interval is read
// per job.
func (h *Channels) putEpicBriefsConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var cfg epicBriefsConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if cfg.MinIntervalMinutes < 1 || cfg.MinIntervalMinutes > 24*60 {
		writeError(w, http.StatusBadRequest, "min_interval_minutes must be between 1 and 1440")
		return
	}
	ctx := r.Context()
	saveSetting(ctx, h.db, epicBriefsEnabledKey, strconv.FormatBool(cfg.Enabled))
	saveSetting(ctx, h.db, epicBriefsMinIntervalKey, strconv.Itoa(cfg.MinIntervalMinutes))
	h.getEpicBriefsConfig(w, r)
}
