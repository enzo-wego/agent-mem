package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/graph/teamdir"
)

// teamBodyCap bounds the team node body in runes. Keyword search reads only
// left(body, 20000), so the cap keeps every line searchable.
const teamBodyCap = 19000

// importTeamDirectoryPayload is the payload of the import_team_directory job:
// the sanitized typed teamdir.Directory. The job is CLI-only (never on the
// /api/graph/jobs/enqueue allowlist) so raw, unsanitized pages are never stored
// in graph.jobs.
type importTeamDirectoryPayload struct {
	Directory json.RawMessage `json:"directory"`
}

// teamImportHooks are test seams; zero values use the real behavior.
type teamImportHooks struct {
	// enqueueIndex queues the index job for one team node.
	enqueueIndex func(ctx context.Context, nodeID string) error
	// beforeCommit runs after every write and immediately before Commit.
	beforeCommit func(ctx context.Context, tx pgx.Tx) error
}

// NewImportTeamDirectoryHandler returns the entry for "import_team_directory".
func NewImportTeamDirectoryHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  importTeamDirectoryHandler(deps, teamImportHooks{}),
		Systems:  []string{},
		PoolSize: 1,
		Lease:    600 * time.Second,
	}
}

// personMatch is the outcome of an exact-name lookup.
type personMatch struct {
	id        int64
	ok        bool
	ambiguous bool
}

// teamPersonMatcher resolves names to graph.people ids inside the import
// transaction. Exact, case-insensitive display_name equality only: the first
// candidate with exactly one active, unmerged, non-bot row wins; otherwise no match.
type teamPersonMatcher struct {
	tx    pgx.Tx
	cache map[string]personMatch
}

func (m *teamPersonMatcher) lookup(ctx context.Context, name string) (personMatch, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return personMatch{}, nil
	}
	if r, ok := m.cache[key]; ok {
		return r, nil
	}
	rows, err := m.tx.Query(ctx, `
		SELECT id FROM graph.people
		WHERE lower(trim(display_name)) = lower(trim($1))
		  AND merged_into IS NULL AND active AND NOT is_bot
		LIMIT 2`, name)
	if err != nil {
		return personMatch{}, fmt.Errorf("person match query: %w", err)
	}
	defer rows.Close()
	var found []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return personMatch{}, fmt.Errorf("person match scan: %w", err)
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		return personMatch{}, fmt.Errorf("person match rows: %w", err)
	}
	r := personMatch{}
	switch len(found) {
	case 1:
		r = personMatch{id: found[0], ok: true}
	case 0:
	default:
		r = personMatch{ambiguous: true}
	}
	m.cache[key] = r
	return r, nil
}

// match tries the candidate names in order; the first unique match wins.
func (m *teamPersonMatcher) match(ctx context.Context, names ...string) (personMatch, error) {
	res := personMatch{}
	for _, n := range names {
		r, err := m.lookup(ctx, n)
		if err != nil {
			return personMatch{}, err
		}
		if r.ok {
			return r, nil
		}
		if r.ambiguous {
			res.ambiguous = true
		}
	}
	return res, nil
}

type teamPair struct {
	person int64
	team   string
}

type teamPairState struct {
	listed bool
	basis  string
	date   string
}

var basisRank = map[string]int{
	teamdir.BasisListed: 0, teamdir.BasisManager: 1, teamdir.BasisDepartment: 2, teamdir.BasisOther: 3,
}

// teamImportStats are the counts logged by one import. Counts only.
type teamImportStats struct {
	teams, members, membersResolved  int
	edgesInserted, edgesExisting     int
	linksUnresolved, bodiesTruncated int
	ptInserted, ptUpdated            int
	namesUnmatched, namesAmbiguous   int
}

func importTeamDirectoryHandler(deps Deps, hooks teamImportHooks) jobs.Handler {
	if hooks.enqueueIndex == nil {
		hooks.enqueueIndex = func(ctx context.Context, nodeID string) error {
			_, err := jobs.Enqueue(ctx, deps.DB, "index_artifact",
				map[string]any{"node_id": nodeID, "force": true, "skip_judging": true},
				jobs.EnqueueOptions{Priority: 7, MachineID: deps.MachineID})
			return err
		}
	}
	return func(ctx context.Context, payload []byte) error {
		var p importTeamDirectoryPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: import_team_directory unmarshal: %v", jobs.ErrFatal, err)
		}
		if t := bytes.TrimSpace(p.Directory); len(t) == 0 || bytes.Equal(t, []byte("null")) {
			return fmt.Errorf("%w: import_team_directory: directory is required", jobs.ErrFatal)
		}
		var dir teamdir.Directory
		if err := json.Unmarshal(p.Directory, &dir); err != nil {
			return fmt.Errorf("%w: import_team_directory: parse directory: %v", jobs.ErrFatal, err)
		}
		if err := dir.Validate(); err != nil {
			return fmt.Errorf("%w: import_team_directory: %v", jobs.ErrFatal, err)
		}
		if deps.DB == nil || deps.Extractor == nil {
			return fmt.Errorf("%w: import_team_directory: db and extractor are required", jobs.ErrFatal)
		}

		st, nodeIDs, err := writeTeamDirectory(ctx, deps, &dir, hooks)
		if err != nil {
			return err
		}

		// Post-commit: queue indexing without topic judging. A failed enqueue is
		// retryable; the upserts are idempotent and duplicate index jobs are harmless.
		for _, id := range nodeIDs {
			if err := hooks.enqueueIndex(ctx, id); err != nil {
				return fmt.Errorf("%w: import_team_directory: enqueue index for %s: %v", jobs.ErrTransient, id, err)
			}
		}
		deps.Logger.Info().
			Int("teams_upserted", st.teams).
			Int("members_total", st.members).Int("members_resolved", st.membersResolved).
			Int("edges_inserted", st.edgesInserted).Int("edges_existing", st.edgesExisting).
			Int("links_unresolved", st.linksUnresolved).
			Int("bodies_truncated", st.bodiesTruncated).
			Int("person_teams_inserted", st.ptInserted).Int("person_teams_updated", st.ptUpdated).
			Int("names_unmatched_incl_inactive", st.namesUnmatched).Int("names_ambiguous", st.namesAmbiguous).
			Msg("import_team_directory: done")
		return nil
	}
}

// writeTeamDirectory runs every write in one transaction and returns the team
// node ids in processing order.
func writeTeamDirectory(ctx context.Context, deps Deps, dir *teamdir.Directory, hooks teamImportHooks) (teamImportStats, []string, error) {
	var st teamImportStats
	tx, err := deps.DB.Begin(ctx)
	if err != nil {
		return st, nil, fmt.Errorf("import_team_directory: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	teams := append([]teamdir.Team(nil), dir.Teams...)
	sort.Slice(teams, func(i, j int) bool { return teams[i].ID < teams[j].ID })

	matcher := &teamPersonMatcher{tx: tx, cache: map[string]personMatch{}}

	// Group associations by team.
	peopleBy := map[string][]teamdir.Person{}
	for _, x := range dir.People {
		peopleBy[x.TeamID] = append(peopleBy[x.TeamID], x)
	}
	reportsBy := map[string][]teamdir.Report{}
	for _, x := range dir.Reports {
		reportsBy[x.TeamID] = append(reportsBy[x.TeamID], x)
	}
	projectsBy := map[string][]teamdir.Project{}
	for _, x := range dir.Projects {
		projectsBy[x.TeamID] = append(projectsBy[x.TeamID], x)
	}
	ownedBy := map[string][]teamdir.PastWork{}
	involvedBy := map[string][]teamdir.PastWork{}
	for _, x := range dir.PastWork {
		ownedBy[x.Owner] = append(ownedBy[x.Owner], x)
		for _, id := range x.Involved {
			involvedBy[id] = append(involvedBy[id], x)
		}
	}

	// Resolve every directory row's person once.
	dirMatch := make([]personMatch, len(dir.Directory))
	for i, m := range dir.Directory {
		if dirMatch[i], err = matcher.match(ctx, m.Name); err != nil {
			return st, nil, err
		}
	}
	rosterBy := map[string]map[string]int{}
	for i, m := range dir.Directory {
		for _, id := range m.Teams {
			r := rosterBy[id]
			if r == nil {
				r = map[string]int{}
				rosterBy[id] = r
			}
			r["total"]++
			r[m.Basis]++
			if dirMatch[i].ok {
				r["matched"]++
			}
		}
	}

	pairs := map[teamPair]*teamPairState{}
	addPair := func(person int64, team, basis, date string, listed bool) {
		k := teamPair{person, team}
		s := pairs[k]
		if s == nil {
			s = &teamPairState{basis: basis, date: date, listed: listed}
			pairs[k] = s
			return
		}
		s.listed = s.listed || listed
		if basisRank[basis] < basisRank[s.basis] {
			s.basis = basis
		}
		if date > s.date {
			s.date = date
		}
	}

	nodeIDs := make([]string, 0, len(teams))
	for _, t := range teams {
		nodeID, err := ids.Team(t.ID)
		if err != nil {
			return st, nil, fmt.Errorf("%w: import_team_directory: %v", jobs.ErrFatal, err)
		}

		// Contacts: resolve person ids for metadata and person_teams.
		type memberMeta struct {
			Name     string `json:"name"`
			Role     string `json:"role"`
			IsLead   bool   `json:"is_lead"`
			PersonID *int64 `json:"person_id"`
		}
		members := []memberMeta{}
		for _, c := range peopleBy[t.ID] {
			r, err := matcher.match(ctx, c.HRName, c.Name)
			if err != nil {
				return st, nil, err
			}
			mm := memberMeta{Name: c.Name, Role: c.Role, IsLead: c.IsLead}
			st.members++
			if r.ok {
				id := r.id
				mm.PersonID = &id
				st.membersResolved++
				addPair(r.id, nodeID, teamdir.BasisListed, c.LastConfirmed, true)
			}
			members = append(members, mm)
		}
		for i, m := range dir.Directory {
			if !dirMatch[i].ok {
				continue
			}
			for _, id := range m.Teams {
				if id == t.ID {
					addPair(dirMatch[i].id, nodeID, m.Basis, m.LastConfirmed, m.Basis == teamdir.BasisListed)
				}
			}
		}

		roster := rosterBy[t.ID]
		body, truncated := buildTeamBody(t, peopleBy[t.ID], roster, reportsBy[t.ID], projectsBy[t.ID], ownedBy[t.ID], involvedBy[t.ID])
		if truncated {
			st.bodiesTruncated++
		}
		meta := map[string]any{
			"source_system":     "tenderloin",
			"members_complete":  false,
			"last_confirmed":    t.LastConfirmed,
			"sources":           t.Source,
			"slack_channels":    nonNil(t.SlackChannels),
			"jira_projects":     nonNil(t.JiraProjects),
			"confluence_spaces": nonNil(t.ConfluenceSpaces),
			"key_systems":       nonNil(t.KeySystems),
			"members":           members,
			"roster": map[string]int{
				"total": roster["total"], "listed": roster[teamdir.BasisListed],
				"manager": roster[teamdir.BasisManager], "department": roster[teamdir.BasisDepartment],
				"other": roster[teamdir.BasisOther], "matched": roster["matched"],
			},
		}
		metaJSON, err := json.Marshal(meta)
		if err != nil {
			return st, nil, fmt.Errorf("import_team_directory: marshal metadata for %s: %w", t.ID, err)
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO graph.nodes (id, type, natural_key, title, body, metadata, scope, body_revision, updated_at, machine_id)
			VALUES ($1, 'team', $2, $3, $4, $5::jsonb, 'public', 0, now(), $6)
			ON CONFLICT (id) DO UPDATE SET title = EXCLUDED.title, body = EXCLUDED.body,
			  metadata = EXCLUDED.metadata, scope = EXCLUDED.scope, updated_at = now()
			WHERE graph.nodes.type = 'team'`,
			nodeID, t.ID, t.Name, body, string(metaJSON), deps.MachineID)
		if err != nil {
			return st, nil, fmt.Errorf("import_team_directory: upsert node %s: %w", nodeID, err)
		}
		if tag.RowsAffected() == 0 {
			return st, nil, fmt.Errorf("%w: import_team_directory: node id %s exists and is not a team node", jobs.ErrFatal, nodeID)
		}
		st.teams++
		nodeIDs = append(nodeIDs, nodeID)

		// Edges, insert-only.
		var links []string
		for _, r := range reportsBy[t.ID] {
			links = append(links, r.Link)
		}
		for _, p := range projectsBy[t.ID] {
			links = append(links, p.Link)
		}
		for _, w := range ownedBy[t.ID] {
			links = append(links, w.Links...)
		}
		if err := insertTeamEdges(ctx, tx, deps, nodeID, links, &st); err != nil {
			return st, nil, err
		}
	}

	// Person→team rows, in sorted order so the write order is stable.
	keys := make([]teamPair, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].team != keys[j].team {
			return keys[i].team < keys[j].team
		}
		return keys[i].person < keys[j].person
	})
	for _, k := range keys {
		s := pairs[k]
		var inserted bool
		err := tx.QueryRow(ctx, `
			INSERT INTO graph.person_teams (person_id, team_node_id, basis, last_confirmed)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (person_id, team_node_id) DO UPDATE
			  SET basis = EXCLUDED.basis,
			      last_confirmed = GREATEST(graph.person_teams.last_confirmed, EXCLUDED.last_confirmed),
			      updated_at = now()
			RETURNING (xmax = 0)`, k.person, k.team, s.basis, s.date).Scan(&inserted)
		if err != nil {
			return st, nil, fmt.Errorf("import_team_directory: upsert person_teams for %s: %w", k.team, err)
		}
		if inserted {
			st.ptInserted++
		} else {
			st.ptUpdated++
		}
	}
	for _, r := range matcher.cache {
		switch {
		case r.ok:
		case r.ambiguous:
			st.namesAmbiguous++
		default:
			st.namesUnmatched++
		}
	}

	if hooks.beforeCommit != nil {
		if err := hooks.beforeCommit(ctx, tx); err != nil {
			return st, nil, fmt.Errorf("import_team_directory: before commit: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return st, nil, fmt.Errorf("import_team_directory: commit: %w", err)
	}
	return st, nodeIDs, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// insertTeamEdges extracts node ids from links and inserts a REFERENCES edge for
// each one that exists. Never updates or deletes an edge.
func insertTeamEdges(ctx context.Context, tx pgx.Tx, deps Deps, from string, links []string, st *teamImportStats) error {
	var targets []string
	seen := map[string]bool{}
	for _, l := range links {
		if strings.TrimSpace(l) == "" {
			continue
		}
		res, err := deps.Extractor.Extract(ctx, l)
		if err != nil {
			return fmt.Errorf("import_team_directory: extract links for %s: %w", from, err)
		}
		for _, f := range res.Findings {
			if f.NodeID == from || seen[f.NodeID] {
				continue
			}
			seen[f.NodeID] = true
			targets = append(targets, f.NodeID)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	sort.Strings(targets)
	rows, err := tx.Query(ctx, `SELECT id FROM graph.nodes WHERE id = ANY($1)`, targets)
	if err != nil {
		return fmt.Errorf("import_team_directory: check link targets for %s: %w", from, err)
	}
	exists := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("import_team_directory: scan link targets: %w", err)
		}
		exists[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("import_team_directory: link targets rows: %w", err)
	}
	for _, to := range targets {
		if !exists[to] {
			st.linksUnresolved++
			continue
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO graph.edges (from_node_id, to_node_id, kind, metadata, machine_id)
			VALUES ($1, $2, 'REFERENCES', '{"method":"tenderloin"}'::jsonb, $3)
			ON CONFLICT (from_node_id, to_node_id, kind) DO NOTHING`, from, to, deps.MachineID)
		if err != nil {
			return fmt.Errorf("import_team_directory: insert edge from %s: %w", from, err)
		}
		if tag.RowsAffected() == 1 {
			st.edgesInserted++
		} else {
			st.edgesExisting++
		}
	}
	return nil
}

var projectStatusRank = map[string]int{"in_progress": 0, "planned": 1, "done": 2, "stopped": 3}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// buildTeamBody renders the team node body. The identifying facts come first
// (the embedder sees only the first ~400 runes). List sections emit one line
// per item so the 19,000-rune cap never cuts inside a line.
func buildTeamBody(t teamdir.Team, contacts []teamdir.Person, roster map[string]int,
	reports []teamdir.Report, projects []teamdir.Project, owned, involved []teamdir.PastWork) (string, bool) {

	var head []string
	var l1 []string
	l1 = append(l1, t.Name+" team.")
	if len(t.SlackChannels) > 0 {
		l1 = append(l1, "Slack: "+strings.Join(t.SlackChannels, ", ")+".")
	}
	if len(t.JiraProjects) > 0 {
		l1 = append(l1, "Jira: "+strings.Join(t.JiraProjects, ", ")+".")
	}
	if len(t.ConfluenceSpaces) > 0 {
		l1 = append(l1, "Confluence: "+strings.Join(t.ConfluenceSpaces, ", ")+".")
	}
	head = append(head, strings.Join(l1, " "))
	head = append(head, "Owns: "+t.Owns, "Mission: "+t.Mission)
	opt := func(label, v string) {
		if strings.TrimSpace(v) != "" {
			head = append(head, label+v)
		}
	}
	opt("Does not own: ", t.DoesNotOwn)
	opt("Key systems: ", strings.Join(t.KeySystems, "; "))
	opt("Sub-teams: ", t.SubTeams)
	opt("Contact: ", t.ContactRole)
	if len(contacts) > 0 {
		var cs []string
		for _, c := range contacts {
			role := c.Role
			if c.IsLead {
				role = joinNonEmpty(", ", role, "lead")
			}
			cs = append(cs, strings.TrimSpace(c.Name+" ("+role+")"))
		}
		head = append(head, "Listed contacts (not a full roster): "+strings.Join(cs, "; "))
	}
	if roster["total"] > 0 {
		head = append(head, fmt.Sprintf("Roster: %d people (%d listed, %d via manager, %d via department)",
			roster["total"], roster[teamdir.BasisListed], roster[teamdir.BasisManager], roster[teamdir.BasisDepartment]))
	}

	var items []string
	for _, r := range reports {
		items = append(items, "Report: "+joinNonEmpty(" ", r.Name+" ("+joinNonEmpty(", ", r.Type, r.Cadence)+")", r.Link))
	}
	ps := append([]teamdir.Project(nil), projects...)
	sort.SliceStable(ps, func(i, j int) bool {
		ri, ok := projectStatusRank[ps[i].Status]
		if !ok {
			ri = len(projectStatusRank)
		}
		rj, ok := projectStatusRank[ps[j].Status]
		if !ok {
			rj = len(projectStatusRank)
		}
		if ri != rj {
			return ri < rj
		}
		return ps[i].Start > ps[j].Start
	})
	for _, p := range ps {
		dates := ""
		if p.Start != "" || p.End != "" {
			dates = "(" + p.Start + "–" + p.End + ")"
		}
		items = append(items, "Project: "+joinNonEmpty(" ", "["+p.Status+"]", joinNonEmpty(": ", p.Title, p.Summary), dates, p.Link))
	}
	for _, w := range owned {
		items = append(items, "Past work (owner): "+joinNonEmpty(" ", joinNonEmpty(": ", w.Title+" ["+w.Type+"]", w.Outcome), strings.Join(w.Links, " ")))
	}
	for _, w := range involved {
		items = append(items, "Past work (involved): "+w.Title)
	}
	tail := ""
	if t.LastConfirmed != "" {
		tail = "Last confirmed: " + t.LastConfirmed
	}

	var b strings.Builder
	used := 0
	add := func(line string) {
		if b.Len() > 0 {
			b.WriteByte('\n')
			used++
		}
		b.WriteString(line)
		used += utf8.RuneCountInString(line)
	}
	for _, l := range head {
		add(l)
	}
	// Reserve room for the overflow line and the tail so neither is ever dropped.
	reserve := utf8.RuneCountInString(tail) + 80
	truncated := false
	for i, it := range items {
		if used+1+utf8.RuneCountInString(it)+reserve > teamBodyCap {
			add(fmt.Sprintf("… %d more items in the Tenderloin team directory", len(items)-i))
			truncated = true
			break
		}
		add(it)
	}
	if tail != "" {
		add(tail)
	}
	return b.String(), truncated
}
