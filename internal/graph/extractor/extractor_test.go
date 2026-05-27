package extractor_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

// -----------------------------------------------------------------------
// Test DB helpers
// -----------------------------------------------------------------------

func openTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("DB ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func truncateEntities(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "DELETE FROM graph.entities"); err != nil {
		t.Fatalf("truncate graph.entities: %v", err)
	}
}

func insertEntity(t *testing.T, pool *pgxpool.Pool, id, kind, displayName string, aliases []string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO graph.entities (id, kind, display_name, aliases, source, machine_id)
		VALUES ($1, $2, $3, $4, 'test', 'local')
		ON CONFLICT (id) DO UPDATE SET aliases = EXCLUDED.aliases`,
		id, kind, displayName, aliases,
	)
	if err != nil {
		t.Fatalf("insertEntity %s: %v", id, err)
	}
}

func newExtractor(t *testing.T, pool *pgxpool.Pool) *extractor.Extractor {
	t.Helper()
	log := zerolog.Nop()
	e := extractor.New(pool, log)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return e
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func mustReadTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return string(b)
}

func findByNodeID(findings []extractor.Finding, nodeID string) *extractor.Finding {
	for i, f := range findings {
		if f.NodeID == nodeID {
			return &findings[i]
		}
	}
	return nil
}

func assertContains(t *testing.T, findings []extractor.Finding, nodeID string) {
	t.Helper()
	if findByNodeID(findings, nodeID) == nil {
		t.Errorf("expected Finding with NodeID=%q not found; got:", nodeID)
		for _, f := range findings {
			t.Errorf("  %s (source=%q, match=%s)", f.NodeID, f.Source, f.Match)
		}
	}
}

func assertNotContains(t *testing.T, findings []extractor.Finding, nodeID string) {
	t.Helper()
	if findByNodeID(findings, nodeID) != nil {
		t.Errorf("unexpected Finding with NodeID=%q found", nodeID)
	}
}

// -----------------------------------------------------------------------
// URL/ID regex tests (no DB required — use nil pool, no entities)
// -----------------------------------------------------------------------

func newExtractorNoEntities(t *testing.T) *extractor.Extractor {
	t.Helper()
	// Pass nil pool — no entity matching will be attempted because the entity
	// cache remains empty (no Refresh call), which is fine for regex-only tests.
	log := zerolog.Nop()
	return extractor.New(nil, log)
}

func TestExtract_SlackArchiveURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "See https://wego.slack.com/archives/C08S954G2LX/p1779710863216389 for context"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// ts "1779710863216389" → "1779710863.216389"
	want := ids.SlackThread("C08S954G2LX", "1779710863.216389")
	assertContains(t, result.Findings, want)
}

func TestExtract_SlackFileURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Attached: https://wego.slack.com/files/U01ABCDEF/F0B5TLXQLTV/screenshot.png"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertContains(t, result.Findings, ids.SlackFile("F0B5TLXQLTV"))
}

func TestExtract_JiraURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "See https://wegomushi.atlassian.net/browse/PAY-2128 for details"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want, _ := ids.Jira("PAY-2128")
	assertContains(t, result.Findings, want)
}

func TestExtract_BareJiraKey(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Tracking issue PAY-2128 — please check."
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want, _ := ids.Jira("PAY-2128")
	assertContains(t, result.Findings, want)
}

func TestExtract_JiraKey_DeduplicatedByURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	// URL match and bare ID match should both resolve to the same NodeID → deduped.
	body := "https://wegomushi.atlassian.net/browse/PAY-2128 and also bare PAY-2128"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want, _ := ids.Jira("PAY-2128")
	count := 0
	for _, f := range result.Findings {
		if f.NodeID == want {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 Finding for PAY-2128 after dedup, got %d", count)
	}
}

func TestExtract_GitHubPRURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Fix landed in https://github.com/wego/payments/pull/1960"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want, _ := ids.GHPR("wego/payments", 1960)
	assertContains(t, result.Findings, want)
	if len(result.GHPRs) == 0 {
		t.Error("expected GHPRs convenience slice to be populated")
	}
}

func TestExtract_PagerDutyURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Incident: https://wegotravel.pagerduty.com/incidents/P8K3M2N"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want, _ := ids.PagerDuty("P8K3M2N")
	assertContains(t, result.Findings, want)
}

func TestExtract_DatadogMonitorURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Monitor: https://app.datadoghq.com/monitors/133274814"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want, _ := ids.Datadog("monitor", 133274814)
	assertContains(t, result.Findings, want)
}

func TestExtract_SentryIssueURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Error: https://sentry.io/wego/payments/issues/4872610293/"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want, _ := ids.Sentry("4872610293")
	assertContains(t, result.Findings, want)
}

func TestExtract_ConfluencePageURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Runbook: https://wegomushi.atlassian.net/wiki/spaces/PAY/pages/987654321"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertContains(t, result.Findings, ids.CFPage(987654321))
}

func TestExtract_GoogleDocURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Doc: https://docs.google.com/document/d/1BxTY9mKPqrs3NuvMnLop/edit"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertContains(t, result.Findings, ids.GWSDoc("1BxTY9mKPqrs3NuvMnLop"))
}

func TestExtract_GoogleDriveFileURL(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "File: https://drive.google.com/file/d/1BxTY9mKPqrs3NuvMnLopXY7QZabcdefgh/view"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertContains(t, result.Findings, ids.GWSDoc("1BxTY9mKPqrs3NuvMnLopXY7QZabcdefgh"))
}

func TestExtract_WegoOrderRef(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Order ref: WF-A1B2C3D4-E5F6-7890 please investigate"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertContains(t, result.Findings, "wego_order:WF-A1B2C3D4-E5F6-7890")
}

func TestExtract_CKOProcessingChannel(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Channel pc_live_abc123xyz was used"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertContains(t, result.Findings, "cko_processing_channel:pc_live_abc123xyz")
}

func TestExtract_PaymentRef(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Payment ref F.k3m9p2q8r was processed"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertContains(t, result.Findings, "payment_ref:F.k3m9p2q8r")
}

func TestExtract_PaymentRef_SkipsFileExtensions(t *testing.T) {
	e := newExtractorNoEntities(t)
	// "F.go" and "F.txt" should NOT match the payment_ref pattern because they
	// look like file extensions. The pattern requires a non-word leading boundary
	// AND lowercase-only after the dot; "go" and "txt" are valid but they typically
	// appear inside identifiers without surrounding whitespace. This test verifies
	// a path like "internal/F.go" does not produce a payment_ref finding.
	body := "See internal/F.go and also F.txt for reference"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, f := range result.Findings {
		if f.Type == ids.NodeType("payment_ref") {
			// Accept only if source has more than 2 chars after "F." (real payment ids)
			if len(f.Source) <= 4 { // "F." + 2 chars (go, tx, etc.)
				t.Errorf("unexpected payment_ref for short suffix: %s", f.Source)
			}
		}
	}
}

func TestExtract_SkipsCodeFencedContent(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Normal text with PAY-1111\n```\ncode block PAY-9999\n```\nAfter fence PAY-2222"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want1, _ := ids.Jira("PAY-1111")
	want2, _ := ids.Jira("PAY-2222")
	skip, _ := ids.Jira("PAY-9999")
	assertContains(t, result.Findings, want1)
	assertContains(t, result.Findings, want2)
	assertNotContains(t, result.Findings, skip)
}

func TestExtract_SkipsIndentedCodeLines(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := "Normal PAY-1111\n    indented PAY-9999 code line\nBack PAY-2222"
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want1, _ := ids.Jira("PAY-1111")
	want2, _ := ids.Jira("PAY-2222")
	skip, _ := ids.Jira("PAY-9999")
	assertContains(t, result.Findings, want1)
	assertContains(t, result.Findings, want2)
	assertNotContains(t, result.Findings, skip)
}

// -----------------------------------------------------------------------
// Testdata file tests (regex only, no DB)
// -----------------------------------------------------------------------

func TestExtract_TryCurrencyLei(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := mustReadTestdata(t, "try_currency_lei.txt")
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Should find the Slack archive URL.
	assertContains(t, result.Findings, ids.SlackThread("C08S954G2LX", "1779710863.216389"))
	// Should find bare Jira key PAY-2128.
	want, _ := ids.Jira("PAY-2128")
	assertContains(t, result.Findings, want)
}

func TestExtract_Pay2128Incident(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := mustReadTestdata(t, "pay2128_incident.txt")
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Jira URL + bare key (deduped to 1).
	jiraID, _ := ids.Jira("PAY-2128")
	assertContains(t, result.Findings, jiraID)

	// PAY-2080 bare key.
	jira2080, _ := ids.Jira("PAY-2080")
	assertContains(t, result.Findings, jira2080)

	// GitHub PR.
	ghPR, _ := ids.GHPR("wego/payments", 1960)
	assertContains(t, result.Findings, ghPR)

	// PagerDuty.
	pd, _ := ids.PagerDuty("P8K3M2N")
	assertContains(t, result.Findings, pd)

	// Sentry.
	sentry, _ := ids.Sentry("4872610293")
	assertContains(t, result.Findings, sentry)

	// Datadog monitor.
	dd, _ := ids.Datadog("monitor", 133274814)
	assertContains(t, result.Findings, dd)

	// Confluence page.
	assertContains(t, result.Findings, ids.CFPage(987654321))

	// Google Drive file.
	assertContains(t, result.Findings, ids.GWSDoc("1BxTY9mKPqrs3NuvMnLopXY7QZabcdefgh"))

	// Slack thread (team thread URL).
	assertContains(t, result.Findings, ids.SlackThread("C05RNSE8TBR", "1779710997.630059"))

	// Slack file URL.
	assertContains(t, result.Findings, ids.SlackFile("F0B5TLXQLTV"))

	// Wego order ref.
	assertContains(t, result.Findings, "wego_order:WF-A1B2C3D4-E5F6-7890")

	// CKO processing channel.
	assertContains(t, result.Findings, "cko_processing_channel:pc_live_abc123xyz")

	// Payment ref.
	assertContains(t, result.Findings, "payment_ref:F.k3m9p2q8r")
}

func TestExtract_TabbyThreadB(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := mustReadTestdata(t, "tabby_thread_b.txt")
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	jiraID, _ := ids.Jira("PAY-2128")
	assertContains(t, result.Findings, jiraID)
	ghPR, _ := ids.GHPR("wego/payments", 1960)
	assertContains(t, result.Findings, ghPR)
}

func TestExtract_TripleASurbhi(t *testing.T) {
	e := newExtractorNoEntities(t)
	body := mustReadTestdata(t, "triplea_surbhi.txt")
	result, err := e.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	jiraID, _ := ids.Jira("PAY-2155")
	assertContains(t, result.Findings, jiraID)
	dd, _ := ids.Datadog("monitor", 133274999)
	assertContains(t, result.Findings, dd)
}

// -----------------------------------------------------------------------
// Entity alias matching tests (requires DB)
// -----------------------------------------------------------------------

func TestExtract_EntityAlias_WordBoundary(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)

	insertEntity(t, pool, "partner:zen", "partner", "Zen", []string{"Zen", "zen"})
	insertEntity(t, pool, "partner:tabby", "partner", "Tabby", []string{"Tabby", "tabby"})

	e := newExtractor(t, pool)
	ctx := context.Background()

	// "frozen" contains "zen" but should NOT match "zen" at a word boundary.
	result, err := e.Extract(ctx, "The frozen payment was processed by tabby")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// tabby should match.
	assertContains(t, result.Findings, "partner:tabby")
	// "frozen" should NOT match "zen" or "partner:zen".
	assertNotContains(t, result.Findings, "partner:zen")
}

func TestExtract_EntityAlias_CaseInsensitive(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)

	insertEntity(t, pool, "partner:triplea", "partner", "TripleA", []string{"TripleA", "3A", "triple a"})

	e := newExtractor(t, pool)
	ctx := context.Background()

	result, err := e.Extract(ctx, "The TRIPLEA payment gateway is delayed")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertContains(t, result.Findings, "partner:triplea")
}

func TestExtract_EntityAlias_MultipleEntities(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)

	insertEntity(t, pool, "partner:tabby", "partner", "Tabby", []string{"Tabby", "tabby"})
	insertEntity(t, pool, "partner:checkout", "partner", "Checkout", []string{"checkout", "ext-wego-checkout"})
	insertEntity(t, pool, "currency:try", "currency", "TRY", []string{"TRY", "try", "Turkish Lira"})

	e := newExtractor(t, pool)
	ctx := context.Background()

	body := "tabby and checkout are failing for TRY transactions"
	result, err := e.Extract(ctx, body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertContains(t, result.Findings, "partner:tabby")
	assertContains(t, result.Findings, "partner:checkout")
	assertContains(t, result.Findings, "currency:try")
}

func TestExtract_EntityAlias_Refresh(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)

	e := newExtractor(t, pool)
	ctx := context.Background()

	// Not yet in DB — should not match.
	result1, err := e.Extract(ctx, "Tabby payment failed")
	if err != nil {
		t.Fatalf("Extract (before insert): %v", err)
	}
	assertNotContains(t, result1.Findings, "partner:tabby")

	// Insert entity then refresh.
	insertEntity(t, pool, "partner:tabby", "partner", "Tabby", []string{"Tabby", "tabby"})
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	result2, err := e.Extract(ctx, "Tabby payment failed")
	if err != nil {
		t.Fatalf("Extract (after refresh): %v", err)
	}
	assertContains(t, result2.Findings, "partner:tabby")
}
