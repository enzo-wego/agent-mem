package handlers

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const teamScoreAsker = 1000

// seedTeamScoreFixture gives the asker a team and department and creates n
// distinct non-zero authors cycling through every scoring path: same team,
// same department, distance <= 2, distance <= 4, and no distance row.
func seedTeamScoreFixture(t *testing.T, pool *pgxpool.Pool, n int) []int {
	t.Helper()
	ctx := context.Background()
	clear := func() {
		for _, q := range []string{`DELETE FROM graph.user_affinity_config`, `DELETE FROM graph.person_distance`} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	clear()
	t.Cleanup(clear)
	if _, err := pool.Exec(ctx, `INSERT INTO graph.user_affinity_config (eeid, team_group_ids, dept_group_ids, machine_id)
		VALUES ($1, '{team-a}', '{dept-a}', 'test')`, teamScoreAsker); err != nil {
		t.Fatal(err)
	}
	var authors []int
	for i := 0; i < n; i++ {
		a := 2000 + i
		authors = append(authors, a)
		var err error
		switch i % 5 {
		case 0: // same team
			_, err = pool.Exec(ctx, `INSERT INTO graph.user_affinity_config (eeid, team_group_ids, dept_group_ids, machine_id) VALUES ($1, '{team-a}', '{}', 'test')`, a)
		case 1: // same department only
			_, err = pool.Exec(ctx, `INSERT INTO graph.user_affinity_config (eeid, team_group_ids, dept_group_ids, machine_id) VALUES ($1, '{team-z}', '{dept-a}', 'test')`, a)
		case 2: // org distance 2
			_, err = pool.Exec(ctx, `INSERT INTO graph.person_distance (a_eeid, b_eeid, hops, lca_eeid) VALUES ($1, $2, 2, 1)`, teamScoreAsker, a)
		case 3: // org distance 4
			_, err = pool.Exec(ctx, `INSERT INTO graph.person_distance (a_eeid, b_eeid, hops, lca_eeid) VALUES ($1, $2, 4, 1)`, teamScoreAsker, a)
		case 4: // no row anywhere: missing distance
		}
		if err != nil {
			t.Fatalf("seed author %d: %v", a, err)
		}
	}
	return authors
}

func TestTeamScoreBatch_MatchesPerRow(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	authors := seedTeamScoreFixture(t, pool, 15)
	// Edge authors: unknown asker/author, the asker themself, and a distance
	// row stored with the larger eeid first in the key order.
	authors = append(authors, 0, teamScoreAsker, 500)
	if _, err := pool.Exec(ctx, `INSERT INTO graph.person_distance (a_eeid, b_eeid, hops, lca_eeid) VALUES (500, $1, 1, 1)`, teamScoreAsker); err != nil {
		t.Fatal(err)
	}
	authors = append(authors, authors[:3]...) // duplicates must be harmless

	for _, asker := range []int{teamScoreAsker, 0} {
		batch := teamScoresForSearch(ctx, pool, asker, authors)
		for _, a := range authors {
			if want := personScoreForSearch(ctx, pool, asker, a); batch[a] != want {
				t.Errorf("asker %d author %d: batch %v, per-row %v", asker, a, batch[a], want)
			}
		}
	}
}

type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestTeamScoreBatch_QueryCount(t *testing.T) {
	setup := openTestDB(t)
	ctx := context.Background()

	counted := func(n int) (perRow, batched int64) {
		authors := seedTeamScoreFixture(t, setup, n)
		cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		counter := &queryCounter{}
		cfg.ConnConfig.Tracer = counter
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		if err := pool.Ping(ctx); err != nil {
			t.Fatal(err)
		}

		counter.n.Store(0)
		for _, a := range authors {
			personScoreForSearch(ctx, pool, teamScoreAsker, a)
		}
		perRow = counter.n.Load()

		counter.n.Store(0)
		teamScoresForSearch(ctx, pool, teamScoreAsker, authors)
		batched = counter.n.Load()
		return perRow, batched
	}

	row10, batch10 := counted(10)
	row50, batch50 := counted(50)
	if row10 < 10 || row50 < 50 {
		t.Errorf("per-row queries = %d (10 ids), %d (50 ids); fixtures must exercise at least one lookup per id", row10, row50)
	}
	if batch10 != batch50 || batch10 > 4 || batch10 == 0 {
		t.Errorf("batched queries = %d (10 ids), %d (50 ids); want equal and in 1..4", batch10, batch50)
	}
	t.Logf("per-row %d/%d queries, batched %d/%d", row10, row50, batch10, batch50)
}
