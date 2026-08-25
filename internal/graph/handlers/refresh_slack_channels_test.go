package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRefreshSlackChannelsBackfill drives the targeted conversations.info
// backfill against an httptest stand-in for the Slack API. No live Slack calls.
//
// Point DATABASE_URL at the agentmem_test scratch database — handler tests
// delete graph rows and this one seeds graph.nodes / graph.slack_channels.
func TestRefreshSlackChannelsBackfill(t *testing.T) {
	// infoReply scripts one conversations.info response for a channel id.
	type infoReply struct {
		status int    // HTTP status (0 => 200)
		name   string // channel.name when ok
		errStr string // slack error string; non-empty => ok:false
	}

	// A shared prefix keeps this test's seeded rows isolable for cleanup and
	// for the backfill's own LIKE 'slack:%' query without colliding with any
	// other test's ids.
	const px = "CBFT"

	cid := func(s string) string { return px + s }

	cases := []struct {
		name string
		// nodeChannels: channel ids that have nodes but (unless in existing) no
		// slack_channels row — the backfill's targets.
		nodeChannels []string
		// existing: channel ids already in slack_channels (must be skipped).
		existing map[string]string
		// listFails: conversations.list returns ok:false ratelimited, so the
		// list pass errors and the job's returned error is non-nil — but the
		// backfill must still run.
		listFails bool
		// info: scripted conversations.info replies keyed by channel id.
		info map[string]infoReply
		// rateLimitFrom: once this many info calls have been made, every
		// further call returns HTTP 429 (models the loop stopping mid-batch).
		rateLimitFrom int

		wantResolved  map[string]string // ids that must be in slack_channels with these names
		wantAbsent    []string          // ids that must NOT be in slack_channels
		wantInfoCalls int
		wantJobErr    bool
	}{
		{
			name:         "resolves names via conversations.info",
			nodeChannels: []string{cid("A1"), cid("A2")},
			info: map[string]infoReply{
				cid("A1"): {name: "alpha"},
				cid("A2"): {name: "bravo"},
			},
			wantResolved:  map[string]string{cid("A1"): "alpha", cid("A2"): "bravo"},
			wantInfoCalls: 2,
		},
		{
			name:         "channel_not_found is skipped without failing the job",
			nodeChannels: []string{cid("B1"), cid("B2")},
			info: map[string]infoReply{
				cid("B1"): {errStr: "channel_not_found"},
				cid("B2"): {name: "bravo"},
			},
			wantResolved:  map[string]string{cid("B2"): "bravo"},
			wantAbsent:    []string{cid("B1")},
			wantInfoCalls: 2,
		},
		{
			name:         "429 stops the batch early",
			nodeChannels: []string{cid("C1"), cid("C2"), cid("C3")},
			info: map[string]infoReply{
				cid("C1"): {name: "one"}, // resolved before the limit hits
			},
			rateLimitFrom: 1, // C1 resolves, then C2 gets 429 and the loop stops
			wantResolved:  map[string]string{cid("C1"): "one"},
			wantAbsent:    []string{cid("C2"), cid("C3")},
			wantInfoCalls: 2,
		},
		{
			name:         "backfill runs even when the list pass fails",
			nodeChannels: []string{cid("D1")},
			listFails:    true,
			info: map[string]infoReply{
				cid("D1"): {name: "delta"},
			},
			wantResolved:  map[string]string{cid("D1"): "delta"},
			wantInfoCalls: 1,
			wantJobErr:    true, // list error is preserved as the job result
		},
		{
			name:         "already-known channels are not re-fetched",
			nodeChannels: []string{cid("E1"), cid("E2")},
			existing:     map[string]string{cid("E1"): "known"},
			info: map[string]infoReply{
				cid("E2"): {name: "echo"},
			},
			wantResolved:  map[string]string{cid("E1"): "known", cid("E2"): "echo"},
			wantInfoCalls: 1, // only E2, never E1
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := openTestDB(t)
			ctx := context.Background()
			truncateGraphHandlerTables(t, pool)
			cleanupTestChannels(t, pool, px)
			t.Cleanup(func() { cleanupTestChannels(t, pool, px) })

			for _, id := range tc.nodeChannels {
				seedChannelNode(t, pool, id)
			}
			for id, name := range tc.existing {
				if _, err := pool.Exec(ctx, `
					INSERT INTO graph.slack_channels (slack_channel_id, name, machine_id)
					VALUES ($1, $2, 'test')
					ON CONFLICT (slack_channel_id) DO UPDATE SET name = EXCLUDED.name`, id, name); err != nil {
					t.Fatalf("seed existing %s: %v", id, err)
				}
			}

			var infoCalls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/conversations.list":
					if tc.listFails {
						_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
						return
					}
					// No channels from the list pass: the backfill is the
					// subject here, and an empty list keeps cases isolated.
					_, _ = w.Write([]byte(`{"ok":true,"channels":[],"response_metadata":{"next_cursor":""}}`))
				case r.URL.Path == "/api/conversations.info":
					infoCalls++
					if tc.rateLimitFrom > 0 && infoCalls > tc.rateLimitFrom {
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
						return
					}
					id := r.URL.Query().Get("channel")
					rep, ok := tc.info[id]
					if !ok {
						t.Errorf("unexpected conversations.info for %q", id)
						_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
						return
					}
					if rep.status != 0 {
						w.WriteHeader(rep.status)
					}
					if rep.errStr != "" {
						_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":false,"error":%q}`, rep.errStr)))
						return
					}
					_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"channel":{"id":%q,"name":%q}}`, id, rep.name)))
				default:
					t.Errorf("unexpected path %q", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			oldBase, oldHTTP := slackAPIBaseURL, slackHTTP
			slackAPIBaseURL, slackHTTP = srv.URL, srv.Client()
			t.Cleanup(func() { slackAPIBaseURL, slackHTTP = oldBase, oldHTTP })

			t.Setenv("SLACK_BOT_TOKEN", "xoxb-test-not-real")

			h := refreshSlackChannelsHandler(testDeps(pool))
			err := h(ctx, nil)
			if tc.wantJobErr && err == nil {
				t.Fatalf("job returned nil; want an error (list pass failed)")
			}
			if !tc.wantJobErr && err != nil {
				t.Fatalf("job returned %v; want nil", err)
			}

			if infoCalls != tc.wantInfoCalls {
				t.Errorf("conversations.info calls = %d; want %d", infoCalls, tc.wantInfoCalls)
			}

			for id, want := range tc.wantResolved {
				got, ok := channelName(t, pool, id)
				if !ok {
					t.Errorf("channel %s missing from slack_channels; want name %q", id, want)
					continue
				}
				if got != want {
					t.Errorf("channel %s name = %q; want %q", id, got, want)
				}
			}
			for _, id := range tc.wantAbsent {
				if _, ok := channelName(t, pool, id); ok {
					t.Errorf("channel %s present in slack_channels; want absent", id)
				}
			}
		})
	}
}

// TestRefreshSlackChannelsBackfill_CapEnforced seeds more unknown channels than
// the per-run cap and pins that exactly channelBackfillBatchCap are fetched,
// the rest deferred to a later run. Ordered ids (ORDER BY 1 in the query) make
// which ids get resolved deterministic.
func TestRefreshSlackChannelsBackfill_CapEnforced(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	const px = "CBFTCAP"
	truncateGraphHandlerTables(t, pool)
	cleanupTestChannels(t, pool, px)
	t.Cleanup(func() { cleanupTestChannels(t, pool, px) })

	total := channelBackfillBatchCap + 5
	var ids []string
	for i := range total {
		id := fmt.Sprintf("%s%03d", px, i)
		ids = append(ids, id)
		seedChannelNode(t, pool, id)
	}
	sort.Strings(ids) // same order as ORDER BY 1

	var infoCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/conversations.list" {
			_, _ = w.Write([]byte(`{"ok":true,"channels":[],"response_metadata":{"next_cursor":""}}`))
			return
		}
		infoCalls++
		id := r.URL.Query().Get("channel")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"channel":{"id":%q,"name":%q}}`, id, "name-"+id)))
	}))
	defer srv.Close()

	oldBase, oldHTTP := slackAPIBaseURL, slackHTTP
	slackAPIBaseURL, slackHTTP = srv.URL, srv.Client()
	t.Cleanup(func() { slackAPIBaseURL, slackHTTP = oldBase, oldHTTP })
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test-not-real")

	h := refreshSlackChannelsHandler(testDeps(pool))
	if err := h(ctx, nil); err != nil {
		t.Fatalf("job returned %v; want nil", err)
	}

	if infoCalls != channelBackfillBatchCap {
		t.Fatalf("conversations.info calls = %d; want cap %d", infoCalls, channelBackfillBatchCap)
	}
	// The first cap ids (sorted) are resolved; the last 5 are deferred.
	for _, id := range ids[:channelBackfillBatchCap] {
		if _, ok := channelName(t, pool, id); !ok {
			t.Errorf("capped channel %s missing; want resolved", id)
		}
	}
	for _, id := range ids[channelBackfillBatchCap:] {
		if _, ok := channelName(t, pool, id); ok {
			t.Errorf("channel %s resolved; want deferred by cap", id)
		}
	}
}

func seedChannelNode(t *testing.T, pool *pgxpool.Pool, channelID string) {
	t.Helper()
	scope := "slack:" + channelID
	nodeID := scope + ":1000.0001"
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO graph.nodes (id, type, natural_key, title, scope, machine_id)
		VALUES ($1, 'slack', $1, 'seed', $2, 'test')
		ON CONFLICT (id) DO NOTHING`, nodeID, scope); err != nil {
		t.Fatalf("seedChannelNode %s: %v", channelID, err)
	}
}

func channelName(t *testing.T, pool *pgxpool.Pool, channelID string) (string, bool) {
	t.Helper()
	var name string
	err := pool.QueryRow(context.Background(),
		`SELECT name FROM graph.slack_channels WHERE slack_channel_id = $1`, channelID).Scan(&name)
	if err != nil {
		return "", false
	}
	return name, true
}

func cleanupTestChannels(t *testing.T, pool *pgxpool.Pool, prefix string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM graph.nodes WHERE scope LIKE 'slack:' || $1 || '%'`, prefix); err != nil {
		t.Fatalf("cleanup nodes: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM graph.slack_channels WHERE slack_channel_id LIKE $1 || '%'`, prefix); err != nil {
		t.Fatalf("cleanup slack_channels: %v", err)
	}
}
