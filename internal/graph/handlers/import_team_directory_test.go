package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/graph/teamdir"
)

// ---- plain (no DB) tests ----

func TestImportTeamDirectory_BadPayloads(t *testing.T) {
	h := importTeamDirectoryHandler(Deps{Logger: zerolog.Nop()}, teamImportHooks{})
	badTeam := `{"directory":{"teams":[{"id":"a","name":"A"}],"people":[{"team_id":"ghost","name":"P"}]}}`
	for name, payload := range map[string]string{
		"bad json":             `{`,
		"missing directory":    `{}`,
		"null directory":       `{"directory":null}`,
		"zero teams":           `{"directory":{"teams":[]}}`,
		"fails Validate":       badTeam,
		"wrong directory type": `{"directory":[1]}`,
	} {
		err := h(context.Background(), []byte(payload))
		if err == nil || !errors.Is(err, jobs.ErrFatal) {
			t.Errorf("%s: err = %v, want fatal", name, err)
		}
	}
}

func TestBuildTeamBody_CapKeepsLinesWhole(t *testing.T) {
	tm := teamdir.Team{ID: "a", Name: "Team A", Mission: "m", Owns: "o", LastConfirmed: "2026-10-21"}
	var projects []teamdir.Project
	for i := range 400 {
		status := "done"
		if i%50 == 0 {
			status = "in_progress"
		}
		projects = append(projects, teamdir.Project{
			TeamID: "a", Title: "Project " + strings.Repeat("x", 5), Status: status,
			Summary: strings.Repeat("filler ", 20), Start: "2026-01-01",
		})
	}
	body, truncated := buildTeamBody(tm, nil, nil, nil, projects, nil, nil)
	if !truncated {
		t.Fatal("expected truncation")
	}
	lines := strings.Split(body, "\n")
	last := lines[len(lines)-2]
	if !strings.HasPrefix(last, "Last confirmed:") && !strings.HasPrefix(lines[len(lines)-1], "Last confirmed:") {
		t.Fatalf("tail missing: %q", lines[len(lines)-1])
	}
	if n := utf8.RuneCountInString(body); n > teamBodyCap {
		t.Fatalf("body %d runes > cap", n)
	}
	var more string
	for _, l := range lines {
		if strings.HasPrefix(l, "… ") {
			more = l
		}
	}
	if !strings.Contains(more, "more items in the Tenderloin team directory") {
		t.Fatalf("overflow line missing: %q", more)
	}
	// Every line is a whole item: head lines or a complete "Project: [...] ... (2026-01-01–)" line.
	inProgress, done := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "Project: ") {
			if !strings.Contains(l, strings.Repeat("filler ", 20)) {
				t.Fatalf("project line cut mid-line: %q", l)
			}
			if strings.HasPrefix(l, "Project: [in_progress]") {
				inProgress = i
			} else if done < 0 {
				done = i
			}
		}
	}
	if inProgress < 0 || done < 0 || inProgress > done {
		t.Fatalf("in_progress must precede done (in_progress=%d done=%d)", inProgress, done)
	}
}

// ---- DB tests ----

func tdDeps(pool *pgxpool.Pool, logw *bytes.Buffer) Deps {
	lg := zerolog.Nop()
	if logw != nil {
		lg = zerolog.New(logw)
	}
	return Deps{DB: pool, Logger: lg, MachineID: "test", Extractor: extractor.New(pool, zerolog.Nop())}
}

func tdRun(deps Deps, d teamdir.Directory, hooks teamImportHooks) error {
	dj, _ := json.Marshal(d)
	payload, _ := json.Marshal(map[string]json.RawMessage{"directory": dj})
	return importTeamDirectoryHandler(deps, hooks)(context.Background(), payload)
}

func tdSeedPeople(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	ids := map[string]int64{}
	ins := func(key, name string, active bool) {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO graph.people (display_name, active, machine_id) VALUES ($1,$2,'test') RETURNING id`, name, active).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[key] = id
	}
	ins("ann", "Ann Unique", true)
	ins("bob1", "Bob Twin", true)
	ins("bob2", "Bob Twin", true)
	ins("cy", "Cy Merged", true)
	ins("di", "Di Gone", false)
	ins("ed", "Ed Hr", true)
	if _, err := pool.Exec(ctx, `UPDATE graph.people SET merged_into=$1 WHERE id=$2`, ids["ann"], ids["cy"]); err != nil {
		t.Fatal(err)
	}
	return ids
}

func tdSeedNode(t *testing.T, pool *pgxpool.Pool, id, typ string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, title, machine_id) VALUES ($1,$2,$1,'seed','test')`, id, typ); err != nil {
		t.Fatal(err)
	}
}

func snap(t *testing.T, pool *pgxpool.Pool, q string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		b.WriteString(s + "\n")
	}
	return b.String()
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const (
	tdLateSentinel = "latesentinelphrase"
	tdPAY1URL      = "https://wegomushi.atlassian.net/browse/PAY-1"
)

// tdDir is the shared fixture. withLink controls the PAY-1 links; mission goes to team a.
func tdDir(withLink bool, mission string) teamdir.Directory {
	var projects []teamdir.Project
	var past []teamdir.PastWork
	if withLink {
		projects = []teamdir.Project{{TeamID: "a", Title: "Proj", Status: "in_progress", Link: tdPAY1URL}}
		past = []teamdir.PastWork{{ID: "w1", Title: "Old thing", Type: "project", Owner: "a", Involved: []string{"b"}, Links: []string{tdPAY1URL}}}
	}
	return teamdir.Directory{
		Teams: []teamdir.Team{
			{ID: "a", Name: "Team A", Mission: mission + " " + strings.Repeat("padding ", 70), Owns: "owned things",
				SlackChannels: []string{"#team-a"}, JiraProjects: []string{"PAY"}, LastConfirmed: "2026-10-01"},
			{ID: "b", Name: "Team B", Mission: "b mission", Owns: "b owns"},
		},
		People: []teamdir.Person{
			{TeamID: "a", Name: "Ann Unique", Role: "Lead", IsLead: true, LastConfirmed: "2026-09-01"},
			{TeamID: "a", Name: "Bob Twin", Role: "Eng"},
			{TeamID: "a", Name: "Cy Merged", Role: "Eng"},
			{TeamID: "a", Name: "Di Gone", Role: "Eng"},
			{TeamID: "a", Name: "Eddie H", HRName: "Ed Hr", Role: "Eng"},
		},
		Directory: []teamdir.Member{
			{Name: "Ann Unique", Teams: []string{"a", "b"}, Basis: teamdir.BasisManager, LastConfirmed: "2026-10-21"},
			{Name: "Bob Twin", Teams: []string{"a"}, Basis: teamdir.BasisManager},
			{Name: "Nobody Here", Teams: []string{"a"}, Basis: teamdir.BasisOther},
			{Name: "Di Gone", Teams: []string{"a"}, Basis: teamdir.BasisDepartment},
			{Name: "Ed Hr", Teams: []string{"a"}, Basis: teamdir.BasisDepartment, LastConfirmed: "2026-10-21"},
		},
		Reports:  []teamdir.Report{{TeamID: "a", Name: tdLateSentinel, Type: "dashboard", Cadence: "weekly"}},
		Projects: projects,
		PastWork: past,
	}
}

type ptRow struct {
	basis, last, first string
}

func ptRows(t *testing.T, pool *pgxpool.Pool) map[[2]string]ptRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT person_id::text, team_node_id, basis, last_confirmed, first_seen_at::text FROM graph.person_teams`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[[2]string]ptRow{}
	for rows.Next() {
		var p, tm string
		var r ptRow
		if err := rows.Scan(&p, &tm, &r.basis, &r.last, &r.first); err != nil {
			t.Fatal(err)
		}
		out[[2]string{p, tm}] = r
	}
	return out
}

func idStr(id int64) string { b, _ := json.Marshal(id); return string(b) }

func TestImportTeamDirectory_WithDB(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	ctx := context.Background()

	people := tdSeedPeople(t, pool)
	tdSeedNode(t, pool, "jira:PAY-1", "jira")
	tdSeedNode(t, pool, "team:legacy", "team")
	tdSeedNode(t, pool, "jira:OTHER-1", "jira")
	if _, err := pool.Exec(ctx, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,machine_id) VALUES ('jira:OTHER-1','jira:PAY-1','REFERENCES','test')`); err != nil {
		t.Fatal(err)
	}
	peopleBefore := snap(t, pool, `SELECT p::text FROM graph.people p ORDER BY id`)
	legacyBefore := snap(t, pool, `SELECT n::text FROM graph.nodes n WHERE id='team:legacy'`)
	otherEdgeBefore := snap(t, pool, `SELECT e::text FROM graph.edges e WHERE from_node_id='jira:OTHER-1'`)

	var logs bytes.Buffer
	deps := tdDeps(pool, &logs)
	if err := tdRun(deps, tdDir(true, "first mission"), teamImportHooks{}); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Nodes.
	for _, id := range []string{"a", "b"} {
		var typ, scope, nk, body string
		if err := pool.QueryRow(ctx, `SELECT type, scope, natural_key, body FROM graph.nodes WHERE id=$1`, "team:"+id).Scan(&typ, &scope, &nk, &body); err != nil {
			t.Fatalf("team:%s: %v", id, err)
		}
		if typ != "team" || scope != "public" || nk != id {
			t.Fatalf("team:%s type=%s scope=%s nk=%s", id, typ, scope, nk)
		}
		if !strings.HasPrefix(body, "Team "+strings.ToUpper(id)+" team.") {
			t.Fatalf("body line 1 = %q", strings.SplitN(body, "\n", 2)[0])
		}
	}
	var membersJSON string
	if err := pool.QueryRow(ctx, `SELECT metadata->'members'::text FROM graph.nodes WHERE id='team:a'`).Scan(&membersJSON); err != nil {
		t.Fatal(err)
	}
	var members []struct {
		Name     string `json:"name"`
		PersonID *int64 `json:"person_id"`
	}
	if err := json.Unmarshal([]byte(membersJSON), &members); err != nil {
		t.Fatal(err)
	}
	gotPID := map[string]bool{}
	for _, m := range members {
		gotPID[m.Name] = m.PersonID != nil
		if m.Name == "Ann Unique" && (m.PersonID == nil || *m.PersonID != people["ann"]) {
			t.Fatalf("Ann person_id = %v", m.PersonID)
		}
		if m.Name == "Eddie H" && (m.PersonID == nil || *m.PersonID != people["ed"]) {
			t.Fatalf("Ed person_id via hr_name = %v", m.PersonID)
		}
	}
	for name, want := range map[string]bool{"Ann Unique": true, "Bob Twin": false, "Cy Merged": false, "Di Gone": false} {
		if gotPID[name] != want {
			t.Errorf("person_id set for %s = %v, want %v", name, gotPID[name], want)
		}
	}

	// Edges: exactly one REFERENCES team:a -> jira:PAY-1, none from team:b.
	if n := count(t, pool, `SELECT count(*) FROM graph.edges WHERE from_node_id='team:a' AND to_node_id='jira:PAY-1' AND kind='REFERENCES' AND metadata->>'method'='tenderloin'`); n != 1 {
		t.Fatalf("team:a edges = %d", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.edges WHERE from_node_id LIKE 'team:%'`); n != 1 {
		t.Fatalf("team edges total = %d, want 1 (owner team only)", n)
	}
	var bBody string
	_ = pool.QueryRow(ctx, `SELECT body FROM graph.nodes WHERE id='team:b'`).Scan(&bBody)
	if !strings.Contains(bBody, "Past work (involved): Old thing") {
		t.Fatalf("team b body lacks involved past work: %q", bBody)
	}
	var aBody string
	_ = pool.QueryRow(ctx, `SELECT body FROM graph.nodes WHERE id='team:a'`).Scan(&aBody)
	if i := strings.Index(aBody, tdLateSentinel); i < 0 || utf8.RuneCountInString(aBody[:i]) < 400 {
		t.Fatalf("late sentinel must sit past rune 400 (idx %d)", i)
	}

	// Index jobs: one per team, no judging.
	for _, id := range []string{"team:a", "team:b"} {
		if n := count(t, pool, `SELECT count(*) FROM graph.jobs WHERE type='index_artifact' AND payload->>'node_id'=$1 AND payload->>'skip_judging'='true' AND payload->>'force'='true'`, id); n != 1 {
			t.Fatalf("%s index jobs = %d", id, n)
		}
	}

	// Untouched.
	if got := snap(t, pool, `SELECT p::text FROM graph.people p ORDER BY id`); got != peopleBefore {
		t.Fatal("graph.people changed")
	}
	if got := snap(t, pool, `SELECT n::text FROM graph.nodes n WHERE id='team:legacy'`); got != legacyBefore {
		t.Fatal("team:legacy changed")
	}
	if got := snap(t, pool, `SELECT e::text FROM graph.edges e WHERE from_node_id='jira:OTHER-1'`); got != otherEdgeBefore {
		t.Fatal("unrelated edge changed")
	}

	// person_teams (AC14).
	pt := ptRows(t, pool)
	if len(pt) != 3 {
		t.Fatalf("person_teams rows = %d: %v", len(pt), pt)
	}
	annA := pt[[2]string{idStr(people["ann"]), "team:a"}]
	annB := pt[[2]string{idStr(people["ann"]), "team:b"}]
	if annA.basis != "listed" || annA.last != "2026-10-21" {
		t.Fatalf("(Ann, a) = %+v", annA)
	}
	if annB.basis != "manager" || annB.last != "2026-10-21" {
		t.Fatalf("(Ann, b) = %+v", annB)
	}
	edA := pt[[2]string{idStr(people["ed"]), "team:a"}]
	if edA.basis != "listed" || edA.last != "2026-10-21" {
		t.Fatalf("(Ed, a) = %+v (contact date '' + directory date must reconcile)", edA)
	}
	for _, k := range []string{"bob1", "bob2", "cy", "di"} {
		for key := range pt {
			if key[0] == idStr(people[k]) {
				t.Errorf("%s must have no person_teams row", k)
			}
		}
	}
	var roster map[string]int
	var rj string
	_ = pool.QueryRow(ctx, `SELECT metadata->'roster'::text FROM graph.nodes WHERE id='team:a'`).Scan(&rj)
	if err := json.Unmarshal([]byte(rj), &roster); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]int{"total": 5, "listed": 0, "manager": 2, "department": 2, "other": 1, "matched": 2} {
		if roster[k] != want {
			t.Errorf("roster[%s] = %d, want %d (%v)", k, roster[k], want, roster)
		}
	}

	// Log: counts only; unmatched includes inactive and merged.
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		_ = json.Unmarshal([]byte(l), &line)
	}
	if line["names_unmatched_incl_inactive"] != float64(3) || line["names_ambiguous"] != float64(1) {
		t.Fatalf("summary log = %v", line)
	}
	if strings.Contains(logs.String(), "Ann Unique") {
		t.Fatal("log names a person")
	}

	// Re-import: edge stays, mission updates, node count unchanged, person_teams additive.
	nodesBefore := count(t, pool, `SELECT count(*) FROM graph.nodes`)
	ptBefore := ptRows(t, pool)
	d2 := tdDir(false, "second mission")
	d2.Directory = []teamdir.Member{{Name: "Ann Unique", Teams: []string{"a"}, Basis: teamdir.BasisManager, LastConfirmed: "2026-10-25"}}
	if err := tdRun(deps, d2, teamImportHooks{}); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.edges WHERE from_node_id='team:a' AND to_node_id='jira:PAY-1'`); n != 1 {
		t.Fatalf("edge lost on re-import: %d", n)
	}
	_ = pool.QueryRow(ctx, `SELECT body FROM graph.nodes WHERE id='team:a'`).Scan(&aBody)
	if !strings.Contains(aBody, "second mission") || strings.Contains(aBody, "first mission") {
		t.Fatalf("mission not updated: %.200s", aBody)
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.nodes`); n != nodesBefore {
		t.Fatalf("nodes %d -> %d", nodesBefore, n)
	}
	ptAfter := ptRows(t, pool)
	if len(ptAfter) != len(ptBefore) {
		t.Fatalf("person_teams rows %d -> %d", len(ptBefore), len(ptAfter))
	}
	if r := ptAfter[[2]string{idStr(people["ann"]), "team:b"}]; r != ptBefore[[2]string{idStr(people["ann"]), "team:b"}] {
		t.Fatalf("(Ann, b) changed: %+v", r)
	}
	r := ptAfter[[2]string{idStr(people["ann"]), "team:a"}]
	if r.basis != "listed" || r.last != "2026-10-25" || r.first != annA.first {
		t.Fatalf("(Ann, a) after re-import = %+v (first_seen %s)", r, annA.first)
	}

	// An older re-import never lowers a date.
	d3 := tdDir(false, "third")
	for i := range d3.People {
		d3.People[i].LastConfirmed = "2026-05-01"
	}
	for i := range d3.Directory {
		d3.Directory[i].LastConfirmed = "2026-05-01"
	}
	before := ptRows(t, pool)
	if err := tdRun(deps, d3, teamImportHooks{}); err != nil {
		t.Fatalf("old re-import: %v", err)
	}
	for k, old := range before {
		if now := ptRows(t, pool)[k]; now.last < old.last {
			t.Errorf("%v last_confirmed went %s -> %s", k, old.last, now.last)
		}
	}
}

func TestImportTeamDirectory_ConflictingDirectoryRows(t *testing.T) {
	orders := map[string][]teamdir.Member{
		"listed first": {
			{Name: "Ann Unique", Teams: []string{"b"}, Basis: teamdir.BasisListed, LastConfirmed: "2026-10-01"},
			{Name: "Ann Unique", Teams: []string{"b"}, Basis: teamdir.BasisManager, LastConfirmed: "2026-10-21"},
		},
		"manager first": {
			{Name: "Ann Unique", Teams: []string{"b"}, Basis: teamdir.BasisManager, LastConfirmed: "2026-10-21"},
			{Name: "Ann Unique", Teams: []string{"b"}, Basis: teamdir.BasisListed, LastConfirmed: "2026-10-01"},
		},
	}
	for name, rows := range orders {
		t.Run(name, func(t *testing.T) {
			pool := openTestDB(t)
			truncateGraphHandlerTables(t, pool)
			t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
			people := tdSeedPeople(t, pool)
			d := tdDir(false, "m")
			d.People = nil
			d.Directory = rows
			if err := tdRun(tdDeps(pool, nil), d, teamImportHooks{}); err != nil {
				t.Fatal(err)
			}
			r := ptRows(t, pool)[[2]string{idStr(people["ann"]), "team:b"}]
			if r.basis != "listed" || r.last != "2026-10-21" {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func TestImportTeamDirectory_CollisionRollsBack(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	tdSeedNode(t, pool, "jira:PAY-1", "jira")
	tdSeedNode(t, pool, "team:zz-collide", "jira")
	before := snap(t, pool, `SELECT n::text FROM graph.nodes n WHERE id='team:zz-collide'`)

	d := teamdir.Directory{
		Teams:    []teamdir.Team{{ID: "zz-collide", Name: "Collide"}, {ID: "aa-first", Name: "First"}},
		Projects: []teamdir.Project{{TeamID: "aa-first", Title: "P", Status: "planned", Link: tdPAY1URL}},
	}
	err := tdRun(tdDeps(pool, nil), d, teamImportHooks{})
	if err == nil || !strings.Contains(err.Error(), "team:zz-collide") {
		t.Fatalf("err = %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.nodes WHERE id='team:aa-first'`); n != 0 {
		t.Fatal("team:aa-first survived rollback")
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.edges WHERE from_node_id='team:aa-first'`); n != 0 {
		t.Fatal("edges survived rollback")
	}
	if got := snap(t, pool, `SELECT n::text FROM graph.nodes n WHERE id='team:zz-collide'`); got != before {
		t.Fatal("colliding node changed")
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.jobs WHERE type='index_artifact'`); n != 0 {
		t.Fatalf("index jobs = %d", n)
	}
}

func TestImportTeamDirectory_LateFailureRollsBackMemberships(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	people := tdSeedPeople(t, pool)
	tdSeedNode(t, pool, "jira:PAY-1", "jira")
	deps := tdDeps(pool, nil)

	d0 := tdDir(true, "m")
	d0.People = nil
	d0.Directory = []teamdir.Member{{Name: "Ann Unique", Teams: []string{"a"}, Basis: teamdir.BasisManager, LastConfirmed: "2026-01-01"}}
	if err := tdRun(deps, d0, teamImportHooks{}); err != nil {
		t.Fatal(err)
	}
	ptBefore := snap(t, pool, `SELECT t::text FROM graph.person_teams t ORDER BY person_id, team_node_id`)
	nodesBefore := snap(t, pool, `SELECT n::text FROM graph.nodes n WHERE type='team' ORDER BY id`)
	edgesBefore := snap(t, pool, `SELECT e::text FROM graph.edges e ORDER BY id`)
	jobsBefore := count(t, pool, `SELECT count(*) FROM graph.jobs WHERE type='index_artifact'`)

	d1 := tdDir(true, "changed mission")
	d1.People = nil
	d1.Directory = []teamdir.Member{
		{Name: "Ann Unique", Teams: []string{"a"}, Basis: teamdir.BasisListed, LastConfirmed: "2026-10-21"},
		{Name: "Ed Hr", Teams: []string{"a"}, Basis: teamdir.BasisManager},
	}
	boom := errors.New("boom")
	err := tdRun(deps, d1, teamImportHooks{beforeCommit: func(context.Context, pgx.Tx) error { return boom }})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := snap(t, pool, `SELECT t::text FROM graph.person_teams t ORDER BY person_id, team_node_id`); got != ptBefore {
		t.Fatalf("person_teams changed:\n%s\nvs\n%s", got, ptBefore)
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.person_teams WHERE person_id=$1`, people["ed"]); n != 0 {
		t.Fatal("Ed Hr has a row")
	}
	if got := snap(t, pool, `SELECT n::text FROM graph.nodes n WHERE type='team' ORDER BY id`); got != nodesBefore {
		t.Fatal("team nodes changed")
	}
	if got := snap(t, pool, `SELECT e::text FROM graph.edges e ORDER BY id`); got != edgesBefore {
		t.Fatal("edges changed")
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.jobs WHERE type='index_artifact'`); n != jobsBefore {
		t.Fatalf("index jobs %d -> %d", jobsBefore, n)
	}
}

func TestImportTeamDirectory_PartialEnqueueRetry(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	tdSeedNode(t, pool, "jira:PAY-1", "jira")
	deps := tdDeps(pool, nil)
	d := tdDir(true, "m")

	calls := 0
	flaky := func(ctx context.Context, nodeID string) error {
		calls++
		if calls > 1 {
			return errors.New("queue down")
		}
		_, err := jobs.Enqueue(ctx, pool, "index_artifact", map[string]any{"node_id": nodeID, "force": true, "skip_judging": true}, jobs.EnqueueOptions{MachineID: "test"})
		return err
	}
	err := tdRun(deps, d, teamImportHooks{enqueueIndex: flaky})
	if err == nil || !jobs.IsRetryable(err) {
		t.Fatalf("err = %v, want retryable", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.nodes WHERE type='team'`); n != 2 {
		t.Fatalf("team nodes = %d", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.jobs WHERE type='index_artifact'`); n != 1 {
		t.Fatalf("index jobs = %d, want 1", n)
	}
	edges := count(t, pool, `SELECT count(*) FROM graph.edges`)

	if err := tdRun(deps, d, teamImportHooks{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, id := range []string{"team:a", "team:b"} {
		if n := count(t, pool, `SELECT count(*) FROM graph.jobs WHERE type='index_artifact' AND payload->>'node_id'=$1`, id); n < 1 {
			t.Fatalf("%s has no index job", id)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.nodes WHERE type='team'`); n != 2 {
		t.Fatalf("team nodes after retry = %d", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.edges`); n != edges {
		t.Fatalf("edges %d -> %d", edges, n)
	}
}

func TestImportTeamDirectory_NoMatchStillSucceeds(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	d := tdDir(false, "m") // no people are seeded
	if err := tdRun(tdDeps(pool, nil), d, teamImportHooks{}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.person_teams`); n != 0 {
		t.Fatalf("person_teams = %d", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM graph.nodes WHERE type='team'`); n != 2 {
		t.Fatalf("team nodes = %d", n)
	}
}

func TestImportTeamDirectory_KeywordSearchFindsLateBodyText(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	if err := tdRun(tdDeps(pool, nil), tdDir(false, "m"), teamImportHooks{}); err != nil {
		t.Fatal(err)
	}
	aclPerson(t, pool, 9001, "asker@example.test")

	s, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/graph/search?q="+tdLateSentinel+"&arms=keyword", nil)
	req.Header.Set("X-Asker-User", "asker@example.test")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []struct {
			NodeID string `json:"node_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, r := range resp.Results {
		if r.NodeID == "team:a" {
			return
		}
	}
	t.Fatalf("team:a not in results: %s", w.Body.String())
}
