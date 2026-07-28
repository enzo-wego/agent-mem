package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/database"
)

func TestDerivePersonRole_PaymentsEngineeringLead(t *testing.T) {
	candidate := personRoleCandidate{
		EEID:          259,
		Department:    "Engineering",
		JobTitle:      "Staff Software Engineer",
		DirectReports: 3,
		GroupHandles:  []string{"payments-geeks", "payments-ops"},
		ChannelCounts: map[string]int{
			"payments-dev":           434,
			"payments-pull-requests": 166,
			"ext-wego-juspay":        25,
			"vat_data_ota_pk":        20,
			"pm-design":              11,
			"github-alerts":          100,
		},
	}
	role, ok := derivePersonRole(candidate)
	if !ok {
		t.Fatal("Lei's evidence did not produce a role")
	}
	if role.Domain != "payments" || role.RoleLabel != "engineering lead" {
		t.Fatalf("role = %s / %s, want payments / engineering lead", role.Domain, role.RoleLabel)
	}
	if role.Evidence.SeniorityTier != "staff" || role.Evidence.DirectReports != 3 {
		t.Fatalf("leadership evidence wrong: %+v", role.Evidence)
	}
	if role.Evidence.ExcludedFeedMessages != 100 {
		t.Fatalf("excluded feed messages = %d, want 100", role.Evidence.ExcludedFeedMessages)
	}
	if math.Abs(role.Evidence.ActivityShare-(645.0/656.0)) > 0.0001 {
		t.Fatalf("activity share = %f, want %f", role.Evidence.ActivityShare, 645.0/656.0)
	}
	if role.Confidence < 0.9 {
		t.Fatalf("confidence = %f, want high-confidence lead", role.Confidence)
	}
}

func TestDerivePersonRole_StrongEngineeringActivity(t *testing.T) {
	role, ok := derivePersonRole(personRoleCandidate{
		EEID:         651,
		Department:   "Engineering",
		JobTitle:     "Senior Software Engineer 2",
		GroupHandles: []string{"payments-geeks"},
		ChannelCounts: map[string]int{
			"payments-dev": 90,
			"mobile":       10,
		},
	})
	if !ok || role.RoleLabel != "engineer" || role.Domain != "payments" {
		t.Fatalf("strong engineering activity role = %+v, ok=%t", role, ok)
	}
}

func TestDerivePersonRole_RequiresTwentyClassifiedMessages(t *testing.T) {
	if role, ok := derivePersonRole(personRoleCandidate{
		EEID:         652,
		Department:   "Engineering",
		JobTitle:     "Senior Software Engineer",
		GroupHandles: []string{"payments-geeks"},
		ChannelCounts: map[string]int{
			"payments-dev": 14,
			"mobile":       6,
		},
	}); ok {
		t.Fatalf("unexpected role from fewer than 20 classified messages: %+v", role)
	}
}

func TestDerivePersonRole_RejectsInsufficientOrNonTechnicalEvidence(t *testing.T) {
	tests := []struct {
		name      string
		candidate personRoleCandidate
	}{
		{
			name: "single report does not outrank IC title",
			candidate: personRoleCandidate{
				EEID: 1, Department: "Engineering", JobTitle: "Tax Specialist",
				DirectReports: 1, GroupHandles: []string{"payments-geeks"},
			},
		},
		{
			name: "business operations is not technical",
			candidate: personRoleCandidate{
				EEID: 2, Department: "Payments", JobTitle: "Supervisor, Payment Operations",
				DirectReports: 3, GroupHandles: []string{"payments-ops"},
			},
		},
		{
			name: "multiple domains without activity are ambiguous",
			candidate: personRoleCandidate{
				EEID: 3, Department: "Engineering", JobTitle: "Engineering Manager",
				DirectReports: 4, GroupHandles: []string{"payments-geeks", "mobile-geeks"},
			},
		},
		{
			name: "company umbrella is not a domain",
			candidate: personRoleCandidate{
				EEID: 4, Department: "Engineering", JobTitle: "Engineering Manager",
				DirectReports: 4, GroupHandles: []string{"wego-geeks"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if role, ok := derivePersonRole(tt.candidate); ok {
				t.Fatalf("unexpected role: %+v", role)
			}
		})
	}
}

func TestActivityDomainClassification(t *testing.T) {
	domains := []string{"payments", "hajj-umrah"}
	for _, channel := range []string{
		"ext-wego-juspay", "ext-wego-triplea-juspay", "ext-wego-checkout",
		"ext-wego-razorpay", "wego-tap", "vat_data_ota_pk", "value-added-tax",
		"proj-india-tax", "taxes-core", "payments-team",
	} {
		if got := classifyActivityDomain(channel, domains); got != "payments" {
			t.Errorf("%s classified as %q, want payments", channel, got)
		}
	}
	if got := classifyActivityDomain("product-hajj-umrah", domains); got != "hajj-umrah" {
		t.Fatalf("hajj channel classified as %q", got)
	}
	for _, feed := range []string{"jira", "github-alerts", "confluence", "task-alerts-production"} {
		if !excludedActivityFeed(feed) {
			t.Errorf("%s should be excluded from activity denominator", feed)
		}
	}
}

// Point AGENT_MEM_TEST_DATABASE_URL at a scratch database, never the dev one.
func TestRecomputePersonRoles_StoresEvidence(t *testing.T) {
	dsn := os.Getenv("AGENT_MEM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENT_MEM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test-derived-role"}
	const (
		eeid      = -202607283
		reportA   = -202607284
		reportB   = -202607285
		groupID   = "STESTDERIVEDROLE"
		channelID = "CTESTDERIVEDROLE"
	)
	cleanup := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM graph.person_derived_roles WHERE machine_id=$1`, deps.MachineID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM graph.nodes WHERE machine_id=$1`, deps.MachineID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM graph.user_affinity_config WHERE eeid=$1`, eeid)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM graph.slack_groups WHERE id=$1`, groupID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM graph.slack_channels WHERE slack_channel_id=$1`, channelID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM graph.people WHERE machine_id=$1`, deps.MachineID)
	}
	cleanup()
	t.Cleanup(cleanup)

	var personID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO graph.people
			(eeid, display_name, department, job_title, machine_id)
		VALUES ($1, 'Derived Role Test', 'Engineering', 'Staff Software Engineer', $2)
		RETURNING id`,
		eeid, deps.MachineID).Scan(&personID); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	for _, reportEEID := range []int{reportA, reportB} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph.people
				(eeid, display_name, reports_to, machine_id)
			VALUES ($1, 'Test Report', $2, $3)`,
			reportEEID, eeid, deps.MachineID); err != nil {
			t.Fatalf("seed direct report: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.slack_groups
			(id, handle, name, member_user_ids, machine_id)
		VALUES ($1, 'payments-geeks', 'Payments Geeks', '{}'::text[], $2)`,
		groupID, deps.MachineID); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.user_affinity_config
			(eeid, team_group_ids, machine_id)
		VALUES ($1, ARRAY[$2]::text[], $3)`,
		eeid, groupID, deps.MachineID); err != nil {
		t.Fatalf("seed affinity: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.slack_channels
			(slack_channel_id, name, machine_id)
		VALUES ($1, 'payments-dev', $2)`,
		channelID, deps.MachineID); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	for i := 0; i < 25; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph.nodes
				(id, type, natural_key, body, author_person_id, scope, machine_id)
			VALUES ($1, 'slack', $1, 'message', $2, 'slack:' || $3, $4)`,
			fmt.Sprintf("slack:%s:role-test-%d", channelID, i),
			personID, channelID, deps.MachineID); err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
	}

	count, err := recomputePersonRoles(ctx, deps)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if count == 0 {
		t.Fatal("recompute stored no roles")
	}

	var domain, label string
	var confidence float64
	var evidenceRaw []byte
	if err := pool.QueryRow(ctx, `
		SELECT domain, role_label, confidence, evidence
		FROM graph.person_derived_roles
		WHERE eeid=$1`,
		eeid).Scan(&domain, &label, &confidence, &evidenceRaw); err != nil {
		t.Fatalf("read derived role: %v", err)
	}
	if domain != "payments" || label != "engineering lead" || confidence < 0.8 {
		t.Fatalf("stored role wrong: domain=%q label=%q confidence=%f", domain, label, confidence)
	}
	var evidence roleEvidence
	if err := json.Unmarshal(evidenceRaw, &evidence); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	if evidence.DirectReports != 2 || evidence.SelectedDomainCount != 25 ||
		evidence.EligibleMessages != 25 || evidence.ActivityShare != 1 {
		t.Fatalf("stored evidence wrong: %+v", evidence)
	}
}
