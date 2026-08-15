package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Person handles GET /api/graph/person?q=<query>&limit=<n>.
//
// It exposes the person data the graph already holds (graph.people from
// BambooHR + lazy identity resolution, graph.person_derived_roles from the role
// job) as a directly look-up-able profile. People are NOT semantically
// searchable — this is a keyed lookup by name, email, employee id, or Slack id.
type Person struct {
	db *pgxpool.Pool
}

// NewPerson creates a new Person handler.
func NewPerson(db *pgxpool.Pool) *Person {
	return &Person{db: db}
}

const (
	personDefaultLimit    = 5
	personMaxLimit        = 20
	personManagerChainCap = 10
	personArtifactLimit   = 10
	personCanonicalCap    = 20
)

var (
	personDigitsRe  = regexp.MustCompile(`^[0-9]+$`)
	personSlackIDRe = regexp.MustCompile(`^U[A-Z0-9]+$`)
)

type personResponse struct {
	People []personProfile `json:"people"`
	Total  int             `json:"total"`
}

type personProfile struct {
	PersonID        int64            `json:"person_id"`
	EEID            *int             `json:"eeid,omitempty"`
	DisplayName     string           `json:"display_name"`
	Email           string           `json:"email,omitempty"`
	SlackUserID     string           `json:"slack_user_id,omitempty"`
	GitHubLogin     string           `json:"github_login,omitempty"`
	JiraAccountID   string           `json:"jira_account_id,omitempty"`
	IsBot           bool             `json:"is_bot"`
	DepthFromRoot   *int             `json:"depth_from_root,omitempty"`
	ManagerChain    []managerRef     `json:"manager_chain"`
	DirectReports   int              `json:"direct_reports"`
	DerivedRole     *derivedRole     `json:"derived_role,omitempty"`
	RecentArtifacts []recentArtifact `json:"recent_artifacts"`
}

type managerRef struct {
	EEID        int    `json:"eeid"`
	DisplayName string `json:"display_name"`
}

type derivedRole struct {
	Domain     string          `json:"domain"`
	RoleLabel  string          `json:"role_label"`
	Confidence float64         `json:"confidence"`
	Evidence   json.RawMessage `json:"evidence,omitempty"`
	ComputedAt string          `json:"computed_at"`
}

type recentArtifact struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title,omitempty"`
	URL       string `json:"url,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

func (h *Person) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q required", http.StatusBadRequest)
		return
	}
	limit := personDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > personMaxLimit {
			http.Error(w, "limit must be between 1 and 20", http.StatusBadRequest)
			return
		}
		limit = parsed
	}

	// Candidate rows may themselves be merged; follow merged_into to the
	// canonical row before hydrating, and de-dup canonical ids so a match on
	// both a merged row and its canonical row does not appear twice.
	candidates, err := h.matchCandidates(ctx, q, limit)
	if err != nil {
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	seen := make(map[int64]bool, len(candidates))
	resp := personResponse{People: []personProfile{}}
	for _, id := range candidates {
		canonicalID, err := h.canonicalPersonID(ctx, id)
		if err != nil {
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		if seen[canonicalID] {
			continue
		}
		seen[canonicalID] = true
		profile, err := h.loadProfile(ctx, canonicalID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		resp.People = append(resp.People, profile)
		if len(resp.People) >= limit {
			break
		}
	}
	resp.Total = len(resp.People)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// matchCandidates resolves q to raw graph.people ids in the plan's match order:
// all-digits → eeid, contains '@' → email (CITEXT), ^U[A-Z0-9]+$ → slack id,
// otherwise a display_name substring (github_login exact as a bonus).
func (h *Person) matchCandidates(ctx context.Context, q string, limit int) ([]int64, error) {
	switch {
	case personDigitsRe.MatchString(q):
		eeid, err := strconv.Atoi(q)
		if err != nil {
			// An all-digit q that overflows int cannot be an eeid; no match.
			return nil, nil
		}
		return h.queryPersonIDs(ctx, `SELECT id FROM graph.people WHERE eeid = $1`, eeid)
	case strings.Contains(q, "@"):
		return h.queryPersonIDs(ctx, `SELECT id FROM graph.people WHERE email = $1`, q)
	case personSlackIDRe.MatchString(q):
		return h.queryPersonIDs(ctx, `SELECT id FROM graph.people WHERE slack_user_id = $1`, q)
	default:
		return h.queryPersonIDs(ctx, `
SELECT id
FROM graph.people
WHERE merged_into IS NULL
  AND (display_name ILIKE '%' || $1 || '%' OR github_login = $1)
ORDER BY is_bot ASC, depth_from_root ASC NULLS LAST
LIMIT $2`, q, limit)
	}
}

func (h *Person) queryPersonIDs(ctx context.Context, sql string, args ...any) ([]int64, error) {
	rows, err := h.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// canonicalPersonID follows merged_into upward to the canonical (unmerged) row.
// Bounded and cycle-safe so a corrupt merge loop cannot hang the request.
func (h *Person) canonicalPersonID(ctx context.Context, id int64) (int64, error) {
	visited := make(map[int64]bool)
	for range personCanonicalCap {
		if visited[id] {
			return id, nil
		}
		visited[id] = true
		var mergedInto *int64
		err := h.db.QueryRow(ctx, `SELECT merged_into FROM graph.people WHERE id = $1`, id).Scan(&mergedInto)
		if err != nil {
			return 0, err
		}
		if mergedInto == nil {
			return id, nil
		}
		id = *mergedInto
	}
	return id, nil
}

func (h *Person) loadProfile(ctx context.Context, personID int64) (personProfile, error) {
	profile := personProfile{
		ManagerChain:    []managerRef{},
		RecentArtifacts: []recentArtifact{},
	}
	var reportsTo *int
	err := h.db.QueryRow(ctx, `
SELECT id, eeid,
       COALESCE(email::text, ''),
       display_name,
       COALESCE(slack_user_id, ''),
       COALESCE(github_login, ''),
       COALESCE(jira_account_id, ''),
       is_bot,
       depth_from_root,
       reports_to
FROM graph.people
WHERE id = $1`, personID).Scan(
		&profile.PersonID, &profile.EEID, &profile.Email, &profile.DisplayName,
		&profile.SlackUserID, &profile.GitHubLogin, &profile.JiraAccountID,
		&profile.IsBot, &profile.DepthFromRoot, &reportsTo)
	if err != nil {
		return personProfile{}, err
	}

	// Manager chain, direct reports, and derived role are all keyed by eeid.
	// People lazily created but never matched to BambooHR have no eeid — they
	// still return, just with an empty chain, zero reports, and no role.
	if profile.EEID != nil {
		chain, err := h.managerChain(ctx, *profile.EEID, reportsTo)
		if err != nil {
			return personProfile{}, err
		}
		profile.ManagerChain = chain

		if err := h.db.QueryRow(ctx, `
SELECT COUNT(*) FROM graph.people WHERE reports_to = $1 AND merged_into IS NULL`,
			*profile.EEID).Scan(&profile.DirectReports); err != nil {
			return personProfile{}, err
		}

		role, ok, err := h.derivedRole(ctx, *profile.EEID)
		if err != nil {
			return personProfile{}, err
		}
		if ok {
			profile.DerivedRole = &role
		}
	}

	artifacts, err := h.recentArtifacts(ctx, personID)
	if err != nil {
		return personProfile{}, err
	}
	profile.RecentArtifacts = artifacts
	return profile, nil
}

// managerChain walks reports_to (an eeid) upward through graph.people. It is
// capped at personManagerChainCap hops and tracks visited eeids (seeded with
// the subject) so a reporting cycle terminates instead of looping forever.
func (h *Person) managerChain(ctx context.Context, subjectEEID int, firstReportsTo *int) ([]managerRef, error) {
	chain := []managerRef{}
	visited := map[int]bool{subjectEEID: true}
	next := firstReportsTo
	for range personManagerChainCap {
		if next == nil {
			break
		}
		managerEEID := *next
		if visited[managerEEID] {
			break
		}
		visited[managerEEID] = true
		var name string
		var reportsTo *int
		err := h.db.QueryRow(ctx, `
SELECT display_name, reports_to
FROM graph.people
WHERE eeid = $1 AND merged_into IS NULL`, managerEEID).Scan(&name, &reportsTo)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		chain = append(chain, managerRef{EEID: managerEEID, DisplayName: name})
		next = reportsTo
	}
	return chain, nil
}

func (h *Person) derivedRole(ctx context.Context, eeid int) (derivedRole, bool, error) {
	var role derivedRole
	var evidence []byte
	var computedAt time.Time
	err := h.db.QueryRow(ctx, `
SELECT domain, role_label, confidence, evidence, computed_at
FROM graph.person_derived_roles
WHERE eeid = $1`, eeid).Scan(&role.Domain, &role.RoleLabel, &role.Confidence, &evidence, &computedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return derivedRole{}, false, nil
	}
	if err != nil {
		return derivedRole{}, false, err
	}
	role.Evidence = json.RawMessage(evidence)
	role.ComputedAt = computedAt.Format(time.RFC3339)
	return role, true, nil
}

func (h *Person) recentArtifacts(ctx context.Context, personID int64) ([]recentArtifact, error) {
	rows, err := h.db.Query(ctx, `
SELECT id, type, COALESCE(url, ''), COALESCE(title, ''), updated_at
FROM graph.nodes
WHERE author_person_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC
LIMIT `+strconv.Itoa(personArtifactLimit), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	artifacts := []recentArtifact{}
	for rows.Next() {
		var a recentArtifact
		var updatedAt time.Time
		if err := rows.Scan(&a.ID, &a.Type, &a.URL, &a.Title, &updatedAt); err != nil {
			return nil, err
		}
		a.UpdatedAt = updatedAt.Format(time.RFC3339)
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}
