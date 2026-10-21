// Package teamdir parses the Tenderloin team directory (a Wego Hub page that
// embeds one strict-JSON object as `const KB = {...};`) into a sanitized,
// typed Directory. Only the fields the graph import needs are kept: HR
// attributes, Slack handles, manager names, change rules and per-row source
// URLs never make it into Directory.
package teamdir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Team basis values stored in graph.person_teams.
const (
	BasisListed     = "listed"
	BasisManager    = "manager"
	BasisDepartment = "department"
	BasisOther      = "other"
)

var reTeamID = regexp.MustCompile(`^[a-z0-9_-]+$`)

// CanonID is the canonical form of every team id token.
func CanonID(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Team is one kb_teams row.
type Team struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Mission          string   `json:"mission"`
	Owns             string   `json:"owns"`
	DoesNotOwn       string   `json:"does_not_own"`
	KeySystems       []string `json:"key_systems"`
	SubTeams         string   `json:"sub_teams"` // free text, not team ids
	SlackChannels    []string `json:"slack_channels"`
	JiraProjects     []string `json:"jira_projects"`
	ConfluenceSpaces []string `json:"confluence_spaces"`
	ContactRole      string   `json:"contact_role"`
	Source           string   `json:"source"` // metadata only, never the body
	LastConfirmed    string   `json:"last_confirmed"`
}

// Person is one kb_people row (a named contact of a team).
type Person struct {
	TeamID        string `json:"team_id"`
	Name          string `json:"name"`
	HRName        string `json:"hr_name"` // used only for person matching
	Role          string `json:"role"`
	IsLead        bool   `json:"is_lead"`
	LastConfirmed string `json:"last_confirmed"`
}

// Member is one kb_directory row (the company roster mapped to teams).
type Member struct {
	Name          string   `json:"name"`
	Teams         []string `json:"teams"`
	Basis         string   `json:"basis"`
	LastConfirmed string   `json:"last_confirmed"`
}

// Report is one kb_reports row.
type Report struct {
	TeamID  string `json:"team_id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Cadence string `json:"cadence"`
	Link    string `json:"link"`
}

// Project is one kb_team_projects row.
type Project struct {
	ID      string `json:"id"`
	TeamID  string `json:"team_id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Link    string `json:"link"`
}

// PastWork is one kb_past_work row.
type PastWork struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Type     string   `json:"type"`
	Summary  string   `json:"summary"`
	Owner    string   `json:"owner"`
	Involved []string `json:"involved"`
	Start    string   `json:"start"`
	End      string   `json:"end"`
	Outcome  string   `json:"outcome"`
	Links    []string `json:"links"`
}

// Directory is the sanitized, typed import payload.
type Directory struct {
	Teams     []Team     `json:"teams"`
	People    []Person   `json:"people"`
	Directory []Member   `json:"directory"`
	Reports   []Report   `json:"reports"`
	Projects  []Project  `json:"projects"`
	PastWork  []PastWork `json:"past_work"`
}

// Stats counts rows Parse skipped or trimmed. Counts only, never names.
type Stats struct {
	SkippedPeople       int
	SkippedReports      int
	SkippedProjects     int
	SkippedPastWork     int // unknown or empty owner_team
	DroppedInvolved     int // unknown involved_teams tokens
	DroppedDirTeams     int // unknown kb_directory team tokens
	SkippedDirectoryRow int // directory rows left with no team
}

// ---- Extract ----

var kbMarker = regexp.MustCompile(`const\s+KB\s*=\s*`)

var errNotStrictJSON = errors.New("teamdir: KB object is not strict JSON (a JS literal is not supported)")

// Extract returns the KB object bytes from the HTML page (`const KB = {...};`)
// or from a bare JSON object.
func Extract(raw []byte) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("teamdir: empty input")
	}
	rest := trimmed
	if trimmed[0] != '{' {
		loc := kbMarker.FindIndex(trimmed)
		if loc == nil {
			return nil, errors.New("teamdir: `const KB = {...}` not found in input")
		}
		rest = trimmed[loc[1]:]
	}
	if len(rest) == 0 || rest[0] != '{' {
		return nil, errors.New("teamdir: KB is not an object")
	}
	var obj json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(rest)).Decode(&obj); err != nil {
		return nil, fmt.Errorf("%w: %v", errNotStrictJSON, err)
	}
	return obj, nil
}

// ---- Parse ----

type kbFile struct {
	Teams []struct {
		TeamID          string `json:"team_id"`
		TeamName        string `json:"team_name"`
		Mission         string `json:"mission"`
		Owns            string `json:"owns"`
		DoesNotOwn      string `json:"does_not_own"`
		KeySystems      string `json:"key_systems"`
		SubTeams        string `json:"sub_teams"`
		SlackChannel    string `json:"slack_channel"`
		JiraProject     string `json:"jira_project"`
		ConfluenceSpace string `json:"confluence_space"`
		ContactRole     string `json:"contact_role"`
		Source          string `json:"source"`
		LastConfirmed   string `json:"last_confirmed"`
	} `json:"kb_teams"`
	People []struct {
		TeamID        string `json:"team_id"`
		Name          string `json:"name"`
		HRName        string `json:"hr_name"`
		Role          string `json:"role"`
		IsLead        string `json:"is_lead"`
		LastConfirmed string `json:"last_confirmed"`
	} `json:"kb_people"`
	Directory []struct {
		Name          string `json:"name"`
		Teams         string `json:"teams"`
		TeamBasis     string `json:"team_basis"`
		LastConfirmed string `json:"last_confirmed"`
	} `json:"kb_directory"`
	Reports []struct {
		TeamID     string `json:"team_id"`
		ReportName string `json:"report_name"`
		ReportType string `json:"report_type"`
		Cadence    string `json:"cadence"`
		Link       string `json:"link"`
	} `json:"kb_reports"`
	Projects []struct {
		ProjectID string `json:"project_id"`
		TeamID    string `json:"team_id"`
		Title     string `json:"title"`
		Status    string `json:"status"`
		Summary   string `json:"summary"`
		StartDate string `json:"start_date"`
		EndDate   string `json:"end_date"`
		Link      string `json:"link"`
	} `json:"kb_team_projects"`
	PastWork []struct {
		ItemID        string `json:"item_id"`
		Title         string `json:"title"`
		ItemType      string `json:"item_type"`
		Summary       string `json:"summary"`
		OwnerTeam     string `json:"owner_team"`
		InvolvedTeams string `json:"involved_teams"`
		StartDate     string `json:"start_date"`
		EndDate       string `json:"end_date"`
		Outcome       string `json:"outcome"`
		Links         string `json:"links"`
	} `json:"kb_past_work"`
}

// Parse decodes KB JSON into a sanitized Directory. Rows pointing at unknown
// teams are dropped and counted in Stats; team-level problems are errors.
func Parse(kb []byte) (*Directory, Stats, error) {
	var st Stats
	var f kbFile
	if err := json.Unmarshal(kb, &f); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			return nil, st, fmt.Errorf("%w: %v", errNotStrictJSON, err)
		}
		return nil, st, fmt.Errorf("teamdir: decode KB: %w", err)
	}
	if len(f.Teams) == 0 {
		return nil, st, errors.New("teamdir: KB has zero teams")
	}

	d := &Directory{}
	known := map[string]bool{}
	for i, t := range f.Teams {
		rawID := strings.TrimSpace(t.TeamID)
		name := strings.TrimSpace(t.TeamName)
		if rawID == "" {
			return nil, st, fmt.Errorf("teamdir: team #%d has an empty team_id", i)
		}
		id := CanonID(rawID)
		if !reTeamID.MatchString(id) {
			return nil, st, fmt.Errorf("teamdir: team #%d has invalid team_id %q", i, id)
		}
		if name == "" {
			return nil, st, fmt.Errorf("teamdir: team %q has an empty team_name", id)
		}
		if known[id] {
			return nil, st, fmt.Errorf("teamdir: duplicate team_id %q", id)
		}
		known[id] = true
		d.Teams = append(d.Teams, Team{
			ID: id, Name: name,
			Mission:          strings.TrimSpace(t.Mission),
			Owns:             strings.TrimSpace(t.Owns),
			DoesNotOwn:       strings.TrimSpace(t.DoesNotOwn),
			KeySystems:       splitList(t.KeySystems),
			SubTeams:         strings.TrimSpace(t.SubTeams),
			SlackChannels:    splitList(t.SlackChannel),
			JiraProjects:     splitList(t.JiraProject),
			ConfluenceSpaces: splitList(t.ConfluenceSpace),
			ContactRole:      strings.TrimSpace(t.ContactRole),
			Source:           strings.TrimSpace(t.Source),
			LastConfirmed:    strings.TrimSpace(t.LastConfirmed),
		})
	}
	sort.Slice(d.Teams, func(i, j int) bool { return d.Teams[i].ID < d.Teams[j].ID })

	for _, p := range f.People {
		id := CanonID(p.TeamID)
		if !known[id] {
			st.SkippedPeople++
			continue
		}
		d.People = append(d.People, Person{
			TeamID: id, Name: strings.TrimSpace(p.Name), HRName: strings.TrimSpace(p.HRName),
			Role: strings.TrimSpace(p.Role), IsLead: parseLead(p.IsLead),
			LastConfirmed: strings.TrimSpace(p.LastConfirmed),
		})
	}
	for _, m := range f.Directory {
		teams, dropped := canonTeams(m.Teams, known, "")
		st.DroppedDirTeams += dropped
		if len(teams) == 0 {
			st.SkippedDirectoryRow++
			continue
		}
		d.Directory = append(d.Directory, Member{
			Name: strings.TrimSpace(m.Name), Teams: teams,
			Basis: NormalizeBasis(m.TeamBasis), LastConfirmed: strings.TrimSpace(m.LastConfirmed),
		})
	}
	for _, r := range f.Reports {
		id := CanonID(r.TeamID)
		if !known[id] {
			st.SkippedReports++
			continue
		}
		d.Reports = append(d.Reports, Report{
			TeamID: id, Name: strings.TrimSpace(r.ReportName), Type: strings.TrimSpace(r.ReportType),
			Cadence: strings.TrimSpace(r.Cadence), Link: strings.TrimSpace(r.Link),
		})
	}
	for _, p := range f.Projects {
		id := CanonID(p.TeamID)
		if !known[id] {
			st.SkippedProjects++
			continue
		}
		d.Projects = append(d.Projects, Project{
			ID: strings.TrimSpace(p.ProjectID), TeamID: id, Title: strings.TrimSpace(p.Title),
			Status: strings.TrimSpace(p.Status), Summary: strings.TrimSpace(p.Summary),
			Start: strings.TrimSpace(p.StartDate), End: strings.TrimSpace(p.EndDate),
			Link: strings.TrimSpace(p.Link),
		})
	}
	for _, w := range f.PastWork {
		owner := CanonID(w.OwnerTeam)
		if !known[owner] {
			st.SkippedPastWork++
			continue
		}
		inv, dropped := canonTeams(w.InvolvedTeams, known, owner)
		st.DroppedInvolved += dropped
		d.PastWork = append(d.PastWork, PastWork{
			ID: strings.TrimSpace(w.ItemID), Title: strings.TrimSpace(w.Title),
			Type: strings.TrimSpace(w.ItemType), Summary: strings.TrimSpace(w.Summary),
			Owner: owner, Involved: inv,
			Start: strings.TrimSpace(w.StartDate), End: strings.TrimSpace(w.EndDate),
			Outcome: strings.TrimSpace(w.Outcome), Links: splitList(w.Links),
		})
	}
	return d, st, nil
}

// splitList splits on ';' only, trims tokens and drops empty ones.
func splitList(s string) []string {
	var out []string
	for _, tok := range strings.Split(s, ";") {
		if tok = strings.TrimSpace(tok); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// canonTeams canonicalizes a ';'-separated team id list, drops unknown tokens
// (returning how many), deduplicates in first-seen order and removes exclude.
func canonTeams(s string, known map[string]bool, exclude string) ([]string, int) {
	var out []string
	seen := map[string]bool{}
	dropped := 0
	for _, tok := range splitList(s) {
		id := CanonID(tok)
		if !known[id] {
			dropped++
			continue
		}
		if id == exclude || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, dropped
}

func parseLead(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "y", "true":
		return true
	}
	return false
}

// NormalizeBasis maps a kb_directory team_basis to one of the four stored
// values. The manager name in `via manager (<name>)` is discarded.
func NormalizeBasis(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	switch {
	case v == "listed in kb":
		return BasisListed
	case strings.HasPrefix(v, "via manager"):
		return BasisManager
	case v == "via department":
		return BasisDepartment
	}
	return BasisOther
}

// Validate runs the team-level checks of Parse and checks that every
// association points at a known team. Any violation is a producer bug.
func (d *Directory) Validate() error {
	if d == nil || len(d.Teams) == 0 {
		return errors.New("teamdir: directory has zero teams")
	}
	known := map[string]bool{}
	for i, t := range d.Teams {
		id := CanonID(t.ID)
		if id == "" || !reTeamID.MatchString(id) || id != t.ID {
			return fmt.Errorf("teamdir: team #%d has invalid team_id", i)
		}
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("teamdir: team %q has an empty name", id)
		}
		if known[id] {
			return fmt.Errorf("teamdir: duplicate team_id %q", id)
		}
		known[id] = true
	}
	check := func(kind, id string) error {
		if !known[id] {
			return fmt.Errorf("teamdir: %s row points at unknown team %q", kind, id)
		}
		return nil
	}
	for _, p := range d.People {
		if err := check("people", p.TeamID); err != nil {
			return err
		}
	}
	for _, r := range d.Reports {
		if err := check("report", r.TeamID); err != nil {
			return err
		}
	}
	for _, p := range d.Projects {
		if err := check("project", p.TeamID); err != nil {
			return err
		}
	}
	for _, w := range d.PastWork {
		if err := check("past_work owner", w.Owner); err != nil {
			return err
		}
		for _, id := range w.Involved {
			if err := check("past_work involved", id); err != nil {
				return err
			}
		}
	}
	for _, m := range d.Directory {
		for _, id := range m.Teams {
			if err := check("directory", id); err != nil {
				return err
			}
		}
		switch m.Basis {
		case BasisListed, BasisManager, BasisDepartment, BasisOther:
		default:
			return fmt.Errorf("teamdir: directory row has invalid basis %q", m.Basis)
		}
	}
	return nil
}
