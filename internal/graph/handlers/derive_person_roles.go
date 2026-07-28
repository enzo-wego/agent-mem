package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

const (
	derivePersonRolesInterval = 24 * time.Hour
	roleRuleVersion           = "v1"
	strongActivityMinMessages = 20
	strongActivityMinShare    = 0.70
)

type personRoleCandidate struct {
	EEID          int
	Department    string
	JobTitle      string
	DirectReports int
	GroupHandles  []string
	ChannelCounts map[string]int
}

type personDerivedRole struct {
	EEID       int
	Domain     string
	RoleLabel  string
	Confidence float64
	Evidence   roleEvidence
}

type roleEvidence struct {
	RuleVersion          string         `json:"rule_version"`
	Department           string         `json:"department"`
	JobTitle             string         `json:"job_title"`
	SeniorityTier        string         `json:"seniority_tier"`
	DirectReports        int            `json:"direct_reports"`
	GroupHandles         []string       `json:"group_handles"`
	CandidateDomains     []string       `json:"candidate_domains"`
	EligibleMessages     int            `json:"eligible_messages"`
	ExcludedFeedMessages int            `json:"excluded_feed_messages"`
	DomainMessageCounts  map[string]int `json:"domain_message_counts"`
	SelectedDomain       string         `json:"selected_domain"`
	SelectedDomainCount  int            `json:"selected_domain_count"`
	ActivityShare        float64        `json:"activity_share"`
	StrongActivity       bool           `json:"strong_activity"`
}

// NewDerivePersonRolesHandler returns the daily evidence-backed role recomputation job.
func NewDerivePersonRolesHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  derivePersonRolesHandler(deps),
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

func derivePersonRolesHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, _ []byte) error {
		// Keep the daily chain alive even if this run fails or its claim context is
		// cancelled. Startup also seeds a missing chain, so a restart self-heals it.
		defer func() {
			scheduleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, err := jobs.Enqueue(scheduleCtx, deps.DB, "derive_person_roles", map[string]any{},
				jobs.EnqueueOptions{
					AvailableAt:  time.Now().Add(derivePersonRolesInterval),
					TargetRunner: "any",
					MachineID:    deps.MachineID,
				}); err != nil {
				deps.Logger.Warn().Err(err).Msg("derive_person_roles: reschedule failed")
			}
		}()

		count, err := recomputePersonRoles(ctx, deps)
		if err != nil {
			return fmt.Errorf("%w: derive_person_roles: %v", jobs.ErrTransient, err)
		}
		deps.Logger.Info().Int("roles", count).Msg("derive_person_roles: done")
		return nil
	}
}

func recomputePersonRoles(ctx context.Context, deps Deps) (int, error) {
	candidates, err := loadPersonRoleCandidates(ctx, deps)
	if err != nil {
		return 0, err
	}

	roles := make([]personDerivedRole, 0, len(candidates))
	for _, candidate := range candidates {
		if role, ok := derivePersonRole(candidate); ok {
			roles = append(roles, role)
		}
	}

	tx, err := deps.DB.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// The table contains only computed values. Replacing it in one transaction removes
	// stale verdicts without exposing an empty or partially-written result to readers.
	if _, err := tx.Exec(ctx, `DELETE FROM graph.person_derived_roles`); err != nil {
		return 0, fmt.Errorf("delete stale roles: %w", err)
	}
	for _, role := range roles {
		evidence, err := json.Marshal(role.Evidence)
		if err != nil {
			return 0, fmt.Errorf("marshal evidence for eeid %d: %w", role.EEID, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO graph.person_derived_roles
				(eeid, domain, role_label, confidence, evidence, computed_at, machine_id)
			VALUES ($1, $2, $3, $4, $5, NOW(), $6)`,
			role.EEID, role.Domain, role.RoleLabel, role.Confidence, evidence, deps.MachineID,
		); err != nil {
			return 0, fmt.Errorf("insert role for eeid %d: %w", role.EEID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(roles), nil
}

func loadPersonRoleCandidates(ctx context.Context, deps Deps) ([]personRoleCandidate, error) {
	rows, err := deps.DB.Query(ctx, `
		SELECT p.eeid,
		       COALESCE(p.department, ''),
		       COALESCE(p.job_title, ''),
		       count(DISTINCT report.id) FILTER (WHERE report.id IS NOT NULL),
		       array_agg(DISTINCT g.handle ORDER BY g.handle)
		FROM graph.people p
		JOIN graph.user_affinity_config a ON a.eeid = p.eeid
		JOIN graph.slack_groups g ON g.id = ANY(a.team_group_ids)
		LEFT JOIN graph.people report
		  ON report.reports_to = p.eeid
		 AND report.merged_into IS NULL
		 AND report.active
		WHERE p.eeid IS NOT NULL
		  AND p.merged_into IS NULL
		  AND p.active
		GROUP BY p.eeid, p.department, p.job_title
		ORDER BY p.eeid`)
	if err != nil {
		return nil, fmt.Errorf("load candidates: %w", err)
	}
	defer rows.Close()

	var candidates []personRoleCandidate
	byEEID := make(map[int]int)
	var eeids []int
	for rows.Next() {
		var c personRoleCandidate
		if err := rows.Scan(&c.EEID, &c.Department, &c.JobTitle, &c.DirectReports, &c.GroupHandles); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		c.ChannelCounts = make(map[string]int)
		candidates = append(candidates, c)
		byEEID[c.EEID] = len(candidates) - 1
		eeids = append(eeids, c.EEID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("candidate rows: %w", err)
	}
	if len(eeids) == 0 {
		return candidates, nil
	}

	activityRows, err := deps.DB.Query(ctx, `
		SELECT p.eeid, COALESCE(c.name, ''), count(*)
		FROM graph.nodes n
		JOIN graph.people p ON p.id = n.author_person_id
		LEFT JOIN graph.slack_channels c
		  ON c.slack_channel_id = replace(n.scope, 'slack:', '')
		WHERE p.eeid = ANY($1)
		  AND n.type IN ('slack', 'slack_thread')
		  AND n.deleted_at IS NULL
		  AND n.scope LIKE 'slack:%'
		GROUP BY p.eeid, c.name`,
		eeids)
	if err != nil {
		return nil, fmt.Errorf("load activity: %w", err)
	}
	defer activityRows.Close()
	for activityRows.Next() {
		var eeid, count int
		var channel string
		if err := activityRows.Scan(&eeid, &channel, &count); err != nil {
			return nil, fmt.Errorf("scan activity: %w", err)
		}
		if index, ok := byEEID[eeid]; ok {
			candidates[index].ChannelCounts[channel] = count
		}
	}
	if err := activityRows.Err(); err != nil {
		return nil, fmt.Errorf("activity rows: %w", err)
	}
	return candidates, nil
}

func derivePersonRole(candidate personRoleCandidate) (personDerivedRole, bool) {
	domains := roleDomains(candidate.GroupHandles)
	evidence := roleEvidence{
		RuleVersion:         roleRuleVersion,
		Department:          strings.TrimSpace(candidate.Department),
		JobTitle:            strings.TrimSpace(candidate.JobTitle),
		SeniorityTier:       seniorityTier(candidate.JobTitle),
		DirectReports:       candidate.DirectReports,
		GroupHandles:        append([]string(nil), candidate.GroupHandles...),
		CandidateDomains:    append([]string(nil), domains...),
		DomainMessageCounts: make(map[string]int),
	}
	if len(domains) == 0 {
		return personDerivedRole{}, false
	}

	for channel, count := range candidate.ChannelCounts {
		if excludedActivityFeed(channel) {
			evidence.ExcludedFeedMessages += count
			continue
		}
		evidence.EligibleMessages += count
		if domain := classifyActivityDomain(channel, domains); domain != "" {
			evidence.DomainMessageCounts[domain] += count
		}
	}

	selected, ok := selectRoleDomain(domains, evidence.DomainMessageCounts)
	if !ok {
		return personDerivedRole{}, false
	}
	evidence.SelectedDomain = selected
	evidence.SelectedDomainCount = evidence.DomainMessageCounts[selected]
	if evidence.EligibleMessages > 0 {
		evidence.ActivityShare = float64(evidence.SelectedDomainCount) / float64(evidence.EligibleMessages)
	}
	evidence.StrongActivity = evidence.SelectedDomainCount >= strongActivityMinMessages &&
		evidence.ActivityShare >= strongActivityMinShare

	// "Technical" is an HR-backed boundary, not an inference from message vocabulary.
	// This keeps Payments operations/product leaders distinct from Engineering.
	if !strings.EqualFold(strings.TrimSpace(candidate.Department), "Engineering") {
		return personDerivedRole{}, false
	}

	roleLabel := ""
	leadershipTitle := evidence.SeniorityTier == "exec" ||
		evidence.SeniorityTier == "director" ||
		evidence.SeniorityTier == "manager"
	switch {
	case candidate.DirectReports >= 2 || (candidate.DirectReports == 1 && leadershipTitle):
		roleLabel = "engineering lead"
	case evidence.StrongActivity:
		roleLabel = "engineer"
	default:
		return personDerivedRole{}, false
	}

	confidence := 0.68 // exact usergroup domain + HR department
	if roleLabel == "engineering lead" {
		confidence += math.Min(float64(candidate.DirectReports), 5) * 0.04
		if evidence.SeniorityTier != "other" {
			confidence += 0.06
		}
		if evidence.StrongActivity {
			confidence += 0.08
		}
	} else {
		confidence += math.Min(evidence.ActivityShare, 1) * 0.24
	}
	confidence = math.Min(confidence, 0.98)

	return personDerivedRole{
		EEID:       candidate.EEID,
		Domain:     selected,
		RoleLabel:  roleLabel,
		Confidence: confidence,
		Evidence:   evidence,
	}, true
}

func roleDomains(handles []string) []string {
	seen := make(map[string]bool)
	var domains []string
	for _, handle := range handles {
		domain := strings.ToLower(strings.TrimSpace(handle))
		switch {
		case strings.HasSuffix(domain, "-geeks"):
			domain = strings.TrimSuffix(domain, "-geeks")
		case strings.HasSuffix(domain, "-ops"):
			domain = strings.TrimSuffix(domain, "-ops")
		default:
			continue
		}
		// "wego" is a company-wide umbrella, not a functional domain.
		if domain == "" || domain == "wego" || seen[domain] {
			continue
		}
		seen[domain] = true
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	return domains
}

func selectRoleDomain(domains []string, activity map[string]int) (string, bool) {
	if len(domains) == 1 {
		return domains[0], true
	}
	best, bestCount, tied := "", 0, false
	for _, domain := range domains {
		count := activity[domain]
		switch {
		case count > bestCount:
			best, bestCount, tied = domain, count, false
		case count == bestCount && count > 0:
			tied = true
		}
	}
	return best, bestCount > 0 && !tied
}

func excludedActivityFeed(channel string) bool {
	channel = normalizeChannelName(channel)
	for _, marker := range []string{"jira", "github", "confluence", "task-alerts-production"} {
		if strings.Contains(channel, marker) {
			return true
		}
	}
	return false
}

func classifyActivityDomain(channel string, domains []string) string {
	channel = normalizeChannelName(channel)
	if channel == "" {
		return ""
	}
	if containsDomain(domains, "payments") && isPaymentsChannel(channel) {
		return "payments"
	}

	longest := append([]string(nil), domains...)
	sort.SliceStable(longest, func(i, j int) bool { return len(longest[i]) > len(longest[j]) })
	for _, domain := range longest {
		if strings.Contains(channel, normalizeChannelName(domain)) {
			return domain
		}
	}
	return ""
}

func containsDomain(domains []string, want string) bool {
	for _, domain := range domains {
		if domain == want {
			return true
		}
	}
	return false
}

func isPaymentsChannel(channel string) bool {
	if strings.Contains(channel, "payment") {
		return true
	}
	for _, marker := range []string{
		"juspay", "triplea", "triple-a", "checkout", "razorpay", "tabby", "wego-tap",
		"vat-data-ota-", "value-added-tax", "proj-india-tax", "taxes-core",
	} {
		if strings.Contains(channel, marker) {
			return true
		}
	}
	return false
}

func normalizeChannelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "#")
	return strings.NewReplacer("_", "-", " ", "-").Replace(s)
}

func seniorityTier(title string) string {
	title = strings.ToLower(strings.TrimSpace(title))
	switch {
	case strings.Contains(title, "chief"),
		strings.Contains(title, "ceo"),
		strings.Contains(title, "vice president"),
		strings.Contains(title, "vp"),
		strings.Contains(title, "head"):
		return "exec"
	case strings.Contains(title, "director"):
		return "director"
	case strings.Contains(title, "manager"), strings.Contains(title, "supervisor"):
		return "manager"
	case strings.Contains(title, "principal"), strings.Contains(title, "staff"):
		return "staff"
	case strings.Contains(title, "senior"):
		return "senior_ic"
	default:
		return "other"
	}
}
