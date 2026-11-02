package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

func humanSigPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openTestDB(t)
	for _, table := range []string{"graph.jobs", "graph.thread_summaries", "graph.edges", "graph.nodes", "graph.people"} {
		if _, err := pool.Exec(context.Background(), "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}
func seedSigPerson(t *testing.T, pool *pgxpool.Pool, name string, bot bool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO graph.people(display_name,is_bot,machine_id) VALUES($1,$2,'test') RETURNING id`, name, bot).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func seedSigMessage(t *testing.T, pool *pgxpool.Pool, channel, root, ts, body, authorJSON string, person any, updated time.Time) {
	t.Helper()
	meta := fmt.Sprintf(`{"ts":%q,"thread_ts":%q,"author":%s}`, ts, root, authorJSON)
	if _, err := pool.Exec(context.Background(), `INSERT INTO graph.nodes(id,type,natural_key,body,scope,metadata,author_person_id,machine_id,updated_at) VALUES($1,'slack',$1,$2,$3,$4::jsonb,$5,'test',$6)`, "slack:"+channel+":"+ts, body, "slack:"+channel, meta, person, updated); err != nil {
		t.Fatal(err)
	}
}
func runSigSummary(t *testing.T, pool *pgxpool.Pool, gem *mockGemini, logger zerolog.Logger, channel, root string, fill bool) {
	t.Helper()
	payload, _ := json.Marshal(summarizeThreadPayload{ChannelID: channel, ThreadTs: root, FillDecisions: fill})
	if err := NewSummarizeThreadHandler(Deps{DB: pool, Gemini: gem, Logger: logger}).Handler(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
}

func TestSummarizeThread_HumanSignatureInvalidation(t *testing.T) {
	pool := humanSigPool(t)
	ctx := context.Background()
	humanAt := time.UnixMilli(1800000000000).UTC()
	botAt := time.UnixMilli(1800000001000).UTC()
	human := seedSigPerson(t, pool, "Human", false)
	bot := seedSigPerson(t, pool, "Bot", true)
	seedSigMessage(t, pool, "C", "100.000001", "100.000001", "human root", `{"display_name":"Human"}`, human, humanAt)
	calls := 0
	gem := &mockGemini{generateResult: func() (string, error) { calls++; return fmt.Sprintf(`{"topic":"T%d","overview":"O"}`, calls), nil }}
	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	runSigSummary(t, pool, gem, logger, "C", "100.000001", false)
	var full, humanSignature, summary string
	var updated time.Time
	if err := pool.QueryRow(ctx, `SELECT signature,human_signature,summary,updated_at FROM graph.thread_summaries WHERE channel_id='C'`).Scan(&full, &humanSignature, &summary, &updated); err != nil {
		t.Fatal(err)
	}
	if humanSignature != threadHumanSignature(1, humanAt.UnixMilli()) {
		t.Fatalf("human signature %q", humanSignature)
	}
	seedSigMessage(t, pool, "C", "100.000001", "100.000002", "person bot reply", `{"display_name":"Bot"}`, bot, botAt)
	runSigSummary(t, pool, gem, logger, "C", "100.000001", false)
	assertSigRowUnchanged(t, pool, full, summary, updated)
	seedSigMessage(t, pool, "C", "100.000001", "100.000003", "metadata bot reply", `{"display_name":"Bot2","is_bot":true}`, human, botAt.Add(time.Second))
	runSigSummary(t, pool, gem, logger, "C", "100.000001", false)
	assertSigRowUnchanged(t, pool, full, summary, updated)
	if calls != 1 {
		t.Fatalf("bot replies calls=%d", calls)
	}
	humanEdit := humanAt.Add(5 * time.Second)
	if _, err := pool.Exec(ctx, `UPDATE graph.nodes SET body='edited human root',updated_at=$1 WHERE id='slack:C:100.000001'`, humanEdit); err != nil {
		t.Fatal(err)
	}
	runSigSummary(t, pool, gem, logger, "C", "100.000001", false)
	if calls != 2 || !strings.Contains(gem.generateUser, "person bot reply") || !strings.Contains(gem.generateUser, "metadata bot reply") {
		t.Fatalf("human edit calls=%d prompt=%s", calls, gem.generateUser)
	}
	if err := pool.QueryRow(ctx, `SELECT human_signature FROM graph.thread_summaries WHERE channel_id='C'`).Scan(&humanSignature); err != nil || humanSignature != threadHumanSignature(1, humanEdit.UnixMilli()) {
		t.Fatalf("edited signature=%q err=%v", humanSignature, err)
	}
	seedSigMessage(t, pool, "C", "100.000001", "100.000004", "human reply", `{"display_name":"Human"}`, human, humanEdit.Add(time.Second))
	runSigSummary(t, pool, gem, logger, "C", "100.000001", false)
	if calls != 3 {
		t.Fatalf("human reply calls=%d", calls)
	}
	if !strings.Contains(logs.String(), `"outcome":"skip","full_signature_changed":true`) {
		t.Fatalf("missing bot skip log: %s", logs.String())
	}
	runSigSummary(t, pool, gem, logger, "C", "100.000001", false)
	if !strings.Contains(logs.String(), `"outcome":"skip","full_signature_changed":false`) {
		t.Fatalf("missing unchanged skip log: %s", logs.String())
	}
}
func assertSigRowUnchanged(t *testing.T, pool *pgxpool.Pool, full, summary string, updated time.Time) {
	t.Helper()
	var a, b string
	var c time.Time
	if err := pool.QueryRow(context.Background(), `SELECT signature,summary,updated_at FROM graph.thread_summaries WHERE channel_id='C'`).Scan(&a, &b, &c); err != nil {
		t.Fatal(err)
	}
	if a != full || b != summary || !c.Equal(updated) {
		t.Fatalf("row changed: %q %q %s", a, b, c)
	}
}

func TestSummarizeThread_BotFlagInputShapes(t *testing.T) {
	pool := humanSigPool(t)
	at := time.UnixMilli(1810000000000).UTC()
	pb := seedSigPerson(t, pool, "PB", true)
	ph := seedSigPerson(t, pool, "PH", false)
	cases := []struct {
		name, author string
		person       any
		want         int
	}{{"missing", `{"display_name":"A"}`, nil, 1}, {"null", `{"display_name":"A","is_bot":null}`, nil, 1}, {"yes", `{"display_name":"A","is_bot":"yes"}`, nil, 1}, {"false", `{"display_name":"A","is_bot":false}`, nil, 1}, {"person true", `{"display_name":"A"}`, pb, 0}, {"metadata true", `{"display_name":"A","is_bot":true}`, ph, 0}}
	gem := &mockGemini{generateResult: func() (string, error) { return `{"topic":"T","overview":"O"}`, nil }}
	for i, tc := range cases {
		root := fmt.Sprintf("%d.000001", 200+i)
		when := at.Add(time.Duration(i) * time.Second)
		seedSigMessage(t, pool, "S", root, root, tc.name, tc.author, tc.person, when)
		runSigSummary(t, pool, gem, zerolog.Nop(), "S", root, false)
		var got string
		if err := pool.QueryRow(context.Background(), `SELECT human_signature FROM graph.thread_summaries WHERE channel_id='S' AND thread_ts=$1`, root).Scan(&got); err != nil {
			t.Fatal(err)
		}
		ms := int64(0)
		if tc.want == 1 {
			ms = when.UnixMilli()
		}
		if want := threadHumanSignature(tc.want, ms); got != want {
			t.Errorf("%s got %q want %q", tc.name, got, want)
		}
	}
}

func TestSummarizeThread_BotOnlyLegacyAndFillDecisions(t *testing.T) {
	pool := humanSigPool(t)
	ctx := context.Background()
	bot := seedSigPerson(t, pool, "Bot", true)
	at := time.UnixMilli(1820000000000).UTC()
	seedSigMessage(t, pool, "B", "300.000001", "300.000001", "bot root", `{"display_name":"Bot"}`, bot, at)
	calls := 0
	gem := &mockGemini{generateResult: func() (string, error) { calls++; return `{"topic":"T","overview":"O","decisions":[]}`, nil }}
	runSigSummary(t, pool, gem, zerolog.Nop(), "B", "300.000001", false)
	var hs string
	if err := pool.QueryRow(ctx, `SELECT human_signature FROM graph.thread_summaries WHERE channel_id='B'`).Scan(&hs); err != nil || hs != "h1:0:0" {
		t.Fatal(hs, err)
	}
	seedSigMessage(t, pool, "B", "300.000001", "300.000002", "bot reply", `{"display_name":"Bot"}`, bot, at.Add(time.Second))
	runSigSummary(t, pool, gem, zerolog.Nop(), "B", "300.000001", false)
	if calls != 1 {
		t.Fatalf("bot-only calls=%d", calls)
	}
	// A legacy row with a matching full signature and an already populated,
	// correctly computed link signature backfills only human_signature.
	seedSigMessage(t, pool, "L", "400.000001", "400.000001", "https://jira/PAY-N", `{"display_name":"Human"}`, nil, at)
	seedSigResource(t, pool, "jira:PAY-N", "PAY-N title", "PAY-N body", "slack:L:400.000001")
	full := threadSummarySignature(1, at.UnixMilli())
	expectedLink := linkSignature(linkedResourceBlock(ctx, Deps{DB: pool}, "L", "400.000001", true))
	if expectedLink == "" {
		t.Fatal("referenced resource produced empty link signature")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.thread_summaries(channel_id,thread_ts,signature,human_signature,link_signature,summary,overview,updated_at) VALUES('L','400.000001',$1,'',$2,'Legacy','O',$3)`, full, expectedLink, at); err != nil {
		t.Fatal(err)
	}
	runSigSummary(t, pool, gem, zerolog.Nop(), "L", "400.000001", false)
	var sig, summary, link string
	var storedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT signature,human_signature,link_signature,summary,updated_at FROM graph.thread_summaries WHERE channel_id='L'`).Scan(&sig, &hs, &link, &summary, &storedAt); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || sig != full || hs != threadHumanSignature(1, at.UnixMilli()) || link != expectedLink || summary != "Legacy" || !storedAt.Equal(at) {
		t.Fatalf("legacy calls=%d sig=%q human=%q link=%q summary=%q at=%s", calls, sig, hs, link, summary, storedAt)
	}
	// Both legacy signature columns empty: skip and backfill both from a real
	// referenced resource, while preserving the cached row.
	seedSigMessage(t, pool, "M", "450.000001", "450.000001", "https://jira/PAY-L", `{"display_name":"Human"}`, nil, at)
	seedSigResource(t, pool, "jira:PAY-L", "PAY-L title", "PAY-L body", "slack:M:450.000001")
	if _, err := pool.Exec(ctx, `INSERT INTO graph.thread_summaries(channel_id,thread_ts,signature,human_signature,link_signature,summary,overview,updated_at) VALUES('M','450.000001',$1,'','','Legacy links','O',$2)`, full, at); err != nil {
		t.Fatal(err)
	}
	expectedBothEmptyLink := linkSignature(linkedResourceBlock(ctx, Deps{DB: pool}, "M", "450.000001", true))
	if expectedBothEmptyLink == "" {
		t.Fatal("both-empty legacy resource produced empty link signature")
	}
	runSigSummary(t, pool, gem, zerolog.Nop(), "M", "450.000001", false)
	if err := pool.QueryRow(ctx, `SELECT signature,human_signature,link_signature,summary,updated_at FROM graph.thread_summaries WHERE channel_id='M'`).Scan(&sig, &hs, &link, &summary, &storedAt); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || sig != full || hs != threadHumanSignature(1, at.UnixMilli()) || link != expectedBothEmptyLink || summary != "Legacy links" || !storedAt.Equal(at) {
		t.Fatalf("both-empty legacy calls=%d sig=%q human=%q link=%q summary=%q at=%s", calls, sig, hs, link, summary, storedAt)
	}
	if _, err := pool.Exec(ctx, `UPDATE graph.thread_summaries SET decisions=NULL WHERE channel_id='B'`); err != nil {
		t.Fatal(err)
	}
	runSigSummary(t, pool, gem, zerolog.Nop(), "B", "300.000001", true)
	if calls != 2 {
		t.Fatalf("fill decisions calls=%d", calls)
	}
}

func seedSigResource(t *testing.T, pool *pgxpool.Pool, id, title, body, msg string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes(id,type,natural_key,title,body,scope,metadata,machine_id) VALUES($1,'jira',$1,$2,$3,'jira','{}','test')`, id, title, body); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.edges(from_node_id,to_node_id,kind,source_msg_id,machine_id) VALUES($1,$2,'REFERENCES',$1,'test')`, msg, id); err != nil {
		t.Fatal(err)
	}
}
func TestSummarizeThread_BotLinkChangesStillRegenerate(t *testing.T) {
	pool := humanSigPool(t)
	ctx := context.Background()
	human := seedSigPerson(t, pool, "Human", false)
	bot := seedSigPerson(t, pool, "Bot", true)
	at := time.UnixMilli(1830000000000).UTC()
	calls := 0
	gem := &mockGemini{generateResult: func() (string, error) { calls++; return `{"topic":"T","overview":"O"}`, nil }}
	seedSigMessage(t, pool, "R", "500.000001", "500.000001", "https://jira/PAY-1", `{"display_name":"Human"}`, human, at)
	seedSigResource(t, pool, "jira:PAY-1", "PAY-1 title", "EXCERPT-SENTINEL resource body", "slack:R:500.000001")
	runSigSummary(t, pool, gem, zerolog.Nop(), "R", "500.000001", false)
	if !strings.Contains(gem.generateUser, "EXCERPT-SENTINEL") {
		t.Fatal("first prompt lacks excerpt")
	}
	var beforeHuman, beforeLink string
	if err := pool.QueryRow(ctx, `SELECT human_signature,link_signature FROM graph.thread_summaries WHERE channel_id='R'`).Scan(&beforeHuman, &beforeLink); err != nil {
		t.Fatal(err)
	}
	seedSigMessage(t, pool, "R", "500.000001", "500.000002", "automated deployment completed successfully", `{"display_name":"Bot"}`, bot, at.Add(time.Second))
	runSigSummary(t, pool, gem, zerolog.Nop(), "R", "500.000001", false)
	var afterHuman, afterLink string
	if err := pool.QueryRow(ctx, `SELECT human_signature,link_signature FROM graph.thread_summaries WHERE channel_id='R'`).Scan(&afterHuman, &afterLink); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || beforeHuman != afterHuman || beforeLink == afterLink || strings.Contains(gem.generateUser, "EXCERPT-SENTINEL") {
		t.Fatalf("excerpt transition calls=%d human=%q/%q link=%q/%q prompt=%s", calls, beforeHuman, afterHuman, beforeLink, afterLink, gem.generateUser)
	}
	seedSigMessage(t, pool, "Q", "600.000001", "600.000001", "substantive human discussion about PAY-2", `{"display_name":"Human"}`, human, at)
	seedSigResource(t, pool, "jira:PAY-2", "PAY-2 title", "body 2", "slack:Q:600.000001")
	runSigSummary(t, pool, gem, zerolog.Nop(), "Q", "600.000001", false)
	if err := pool.QueryRow(ctx, `SELECT link_signature FROM graph.thread_summaries WHERE channel_id='Q'`).Scan(&beforeLink); err != nil {
		t.Fatal(err)
	}
	seedSigMessage(t, pool, "Q", "600.000001", "600.000002", "https://jira/PAY-3", `{"display_name":"Bot"}`, bot, at.Add(time.Second))
	seedSigResource(t, pool, "jira:PAY-3", "PAY-3 second resource", "body 3", "slack:Q:600.000002")
	runSigSummary(t, pool, gem, zerolog.Nop(), "Q", "600.000001", false)
	if err := pool.QueryRow(ctx, `SELECT link_signature FROM graph.thread_summaries WHERE channel_id='Q'`).Scan(&afterLink); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || beforeLink == afterLink || !strings.Contains(gem.generateUser, "PAY-3 second resource") {
		t.Fatalf("second resource calls=%d link=%q/%q prompt=%s", calls, beforeLink, afterLink, gem.generateUser)
	}
}
