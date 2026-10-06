package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// Settings behind the purpose-line job (dashboard-editable, Settings → Purpose
// line). Off by default. A non-empty allowlist (comma-separated node ids) is
// the canary mode: only listed nodes are queued or processed.
const (
	purposeEnabledKey   = "graph.purpose.enabled"
	purposeAllowlistKey = "graph.purpose.allowlist"
)

// purposeSigVersion prefixes every signature; bump it (with a prompt change) to
// make every stored purpose stale.
const purposeSigVersion = "v3"

const (
	purposeBodyRunes = 6000 // body code points fed to the model and the signature
	purposeMaxRunes  = 160  // hard limit enforced by the parser; the prompt targets 140
)

const purposeSystemPrompt = `You write the purpose line for a Jira ticket, Confluence page or GitHub pull request.
Reply with JSON only: {"purpose": "..."}
The purpose is ONE plain English sentence, ideally at most 20 words and 140 characters.
It says WHY the item exists: the outcome, decision or effect it is for. The reader already sees the title, so do not repeat or paraphrase it; say what the title does not.
Start with a verb or "So that". No markdown, no line breaks, no ticket keys.
Use only facts stated in the text. Do not add numbers, counts, money amounts, team or audience names, or goals that the text does not state. If unsure, leave the detail out.
If the text gives no purpose beyond the title, return {"purpose": ""}.`

var purposeTypes = map[string]bool{"jira": true, "cf": true, "gh_pr": true}

var purposeSpaceRun = regexp.MustCompile(`[ \t]+`)

type summarizePurposePayload struct {
	NodeID string `json:"node_id"`
}

// NewSummarizePurposeHandler returns the job entry for "summarize_purpose".
// Lease is SummaryLease: the default 60 s is shorter than a gateway call and
// would let the job be reclaimed and call the model twice.
func NewSummarizePurposeHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  summarizePurposeHandler(deps),
		Systems:  []string{"gemini"},
		PoolSize: 2,
		Lease:    SummaryLease,
		UsesLLM:  true,
	}
}

func purposeEnabled(ctx context.Context, db *pgxpool.Pool) bool {
	v, _ := strconv.ParseBool(strings.TrimSpace(loadSetting(ctx, db, purposeEnabledKey)))
	return v
}

// purposeAllowed reports whether the node may be queued or processed: setting
// on, and the allowlist empty or containing the node id.
func purposeAllowed(ctx context.Context, db *pgxpool.Pool, nodeID string) bool {
	if !purposeEnabled(ctx, db) {
		return false
	}
	list := strings.TrimSpace(loadSetting(ctx, db, purposeAllowlistKey))
	if list == "" {
		return true
	}
	for _, id := range strings.Split(list, ",") {
		if strings.TrimSpace(id) == nodeID {
			return true
		}
	}
	return false
}

// purposeInput cuts body to its first purposeBodyRunes code points.
func purposeInput(body string) string {
	n := 0
	for i := range body {
		if n == purposeBodyRunes {
			return body[:i]
		}
		n++
	}
	return body
}

func purposeSignature(title, bodyInput string) string {
	sum := sha256.Sum256([]byte(title + "\n" + bodyInput))
	return purposeSigVersion + ":" + hex.EncodeToString(sum[:])
}

// parsePurposeOutput validates the raw model text (see the plan, step 5).
func parsePurposeOutput(raw string) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &obj); err != nil || obj == nil {
		return "", false
	}
	field, ok := obj["purpose"]
	if !ok {
		return "", false
	}
	var v any
	if err := json.Unmarshal(field, &v); err != nil {
		return "", false
	}
	s, isStr := v.(string)
	if !isStr {
		return "", false // null, number, object…
	}
	if strings.ContainsAny(s, "\n\r") {
		return "", false
	}
	s = strings.TrimSpace(purposeSpaceRun.ReplaceAllString(strings.TrimSpace(s), " "))
	if utf8.RuneCountInString(s) > purposeMaxRunes {
		return "", false
	}
	return s, true
}

const purposeReadSQL = `
SELECT COALESCE(n.title,''), COALESCE(ab.body_full, n.body, '')
FROM graph.nodes n
LEFT JOIN graph.artifact_bodies ab ON ab.node_id = n.id
WHERE n.id = $1 AND n.deleted_at IS NULL AND n.type IN ('jira','cf','gh_pr')`

func summarizePurposeHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p summarizePurposePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: summarize_purpose unmarshal: %v", jobs.ErrFatal, err)
		}
		if p.NodeID == "" {
			return fmt.Errorf("%w: summarize_purpose: node_id required", jobs.ErrFatal)
		}
		if deps.Gemini == nil || !purposeAllowed(ctx, deps.DB, p.NodeID) {
			return nil
		}

		var title, body string
		err := deps.DB.QueryRow(ctx, purposeReadSQL, p.NodeID).Scan(&title, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("summarize_purpose: read node: %w", err)
		}
		if strings.TrimSpace(body) == "" {
			return nil // a title alone does not say what an item is for
		}
		bodyIn := purposeInput(body)
		sig := purposeSignature(title, bodyIn)

		var storedSig, failedSig string
		err = deps.DB.QueryRow(ctx,
			`SELECT signature, failed_signature FROM graph.artifact_purposes WHERE node_id = $1`, p.NodeID,
		).Scan(&storedSig, &failedSig)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("summarize_purpose: read row: %w", err)
		}
		if err == nil && (storedSig == sig || failedSig == sig) {
			return nil
		}

		raw, err := deps.Gemini.GenerateCheap(ctx, purposeSystemPrompt, "Title: "+title+"\n\nText:\n"+bodyIn)
		if err != nil {
			return fmt.Errorf("summarize_purpose: model call: %w", err)
		}
		purpose, valid := parsePurposeOutput(raw)

		tx, err := deps.DB.Begin(ctx)
		if err != nil {
			return fmt.Errorf("summarize_purpose: begin: %w", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('purpose:' || $1))`, p.NodeID); err != nil {
			return fmt.Errorf("summarize_purpose: lock: %w", err)
		}
		var curTitle, curBody string
		err = tx.QueryRow(ctx, purposeReadSQL, p.NodeID).Scan(&curTitle, &curBody)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.Commit(ctx) // deleted or retyped during the call
		}
		if err != nil {
			return fmt.Errorf("summarize_purpose: re-read node: %w", err)
		}
		if purposeSignature(curTitle, purposeInput(curBody)) != sig {
			return tx.Commit(ctx) // text changed while the model ran
		}
		if valid {
			_, err = tx.Exec(ctx, `
INSERT INTO graph.artifact_purposes (node_id, purpose, signature, failed_signature, updated_at)
VALUES ($1, $2, $3, '', now())
ON CONFLICT (node_id) DO UPDATE SET purpose = EXCLUDED.purpose, signature = EXCLUDED.signature,
  failed_signature = '', updated_at = now()`, p.NodeID, purpose, sig)
		} else {
			deps.Logger.Warn().Str("node_id", p.NodeID).Msg("summarize_purpose: invalid model output")
			// A stored sentence from an older version must not survive an invalid
			// current-version answer; a same-version one stays.
			_, err = tx.Exec(ctx, `
INSERT INTO graph.artifact_purposes (node_id, failed_signature, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (node_id) DO UPDATE SET failed_signature = EXCLUDED.failed_signature, updated_at = now(),
  purpose = CASE WHEN starts_with(graph.artifact_purposes.signature, $3) THEN graph.artifact_purposes.purpose ELSE '' END,
  signature = CASE WHEN starts_with(graph.artifact_purposes.signature, $3) THEN graph.artifact_purposes.signature ELSE '' END`,
				p.NodeID, sig, purposeSigVersion+":")
		}
		if err != nil {
			return fmt.Errorf("summarize_purpose: store: %w", err)
		}
		return tx.Commit(ctx)
	}
}

// enqueuePurpose queues one summarize_purpose job for a Jira/Confluence/PR node
// unless the setting is off, the allowlist excludes it, or one is already
// queued. Errors are logged, never returned: index_artifact must not fail on it.
func enqueuePurpose(ctx context.Context, deps Deps, nodeID, nodeType string) {
	if deps.DB == nil || !purposeTypes[nodeType] || !purposeAllowed(ctx, deps.DB, nodeID) {
		return
	}
	raw, err := json.Marshal(summarizePurposePayload{NodeID: nodeID})
	if err != nil {
		return
	}
	if _, err := deps.DB.Exec(ctx, `
INSERT INTO graph.jobs (type, payload, priority, machine_id)
SELECT 'summarize_purpose', $2::jsonb, 5, $3
WHERE NOT EXISTS (SELECT 1 FROM graph.jobs
                  WHERE type = 'summarize_purpose' AND status = 'queued'
                    AND payload->>'node_id' = $1)`, nodeID, raw, deps.MachineID); err != nil {
		deps.Logger.Warn().Err(err).Str("node_id", nodeID).Msg("enqueue summarize_purpose failed")
	}
}

// purposeConfig is the Settings → Purpose line payload.
type purposeConfig struct {
	Enabled   bool   `json:"enabled"`
	Allowlist string `json:"allowlist"`
}

// getPurposeConfig serves GET /api/graph/purpose.
func (h *Channels) getPurposeConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	writeJSON(w, http.StatusOK, purposeConfig{
		Enabled:   purposeEnabled(ctx, h.db),
		Allowlist: strings.TrimSpace(loadSetting(ctx, h.db, purposeAllowlistKey)),
	})
}

// putPurposeConfig serves PUT /api/graph/purpose.
func (h *Channels) putPurposeConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var cfg purposeConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	var ids []string
	for _, id := range strings.Split(cfg.Allowlist, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	ctx := r.Context()
	saveSetting(ctx, h.db, purposeEnabledKey, strconv.FormatBool(cfg.Enabled))
	saveSetting(ctx, h.db, purposeAllowlistKey, strings.Join(ids, ","))
	h.getPurposeConfig(w, r)
}
