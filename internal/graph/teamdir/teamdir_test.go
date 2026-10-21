package teamdir

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func team(id, name string) string {
	return `{"team_id":` + jq(id) + `,"team_name":` + jq(name) + `,"mission":"m","owns":"o","slack_channel":"#a; #b","jira_project":"PAY; OPS","confluence_space":"","key_systems":"x, y; z","source":"https://example.test/src"}`
}

func jq(s string) string { b, _ := json.Marshal(s); return string(b) }

func kb(extra string) string {
	return `{"kb_teams":[` + team("payments", "Payments") + `,` + team("b", "Team B") + `,` + team("a", "Team A") + `]` + extra + `}`
}

func mustParse(t *testing.T, s string) (*Directory, Stats) {
	t.Helper()
	d, st, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return d, st
}

func TestExtract(t *testing.T) {
	obj := kb("")
	html := "<html><script>const KB = " + obj + ";\nconst other = {'x': 1};\nfoo();</script></html>"
	for name, in := range map[string]string{"html": html, "bare": obj, "bare-ws": "\n  " + obj + "\n"} {
		got, err := Extract([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != obj {
			t.Fatalf("%s: got %.40s", name, got)
		}
		if _, _, err := Parse(got); err != nil {
			t.Fatalf("%s parse: %v", name, err)
		}
	}
	if _, err := Extract([]byte("<html>no data</html>")); err == nil {
		t.Fatal("missing KB must fail")
	}
}

func TestExtractJSLiteralFails(t *testing.T) {
	_, err := Extract([]byte("<script>const KB = {'kb_teams': []};</script>"))
	if err == nil || !strings.Contains(err.Error(), "not strict JSON") {
		t.Fatalf("err = %v", err)
	}
	_, _, err = Parse([]byte(`{'kb_teams': []}`))
	if err == nil || !strings.Contains(err.Error(), "not strict JSON") {
		t.Fatalf("Parse err = %v", err)
	}
}

func TestParseTeamValidation(t *testing.T) {
	cases := map[string]string{
		"zero teams":     `{"kb_teams":[]}`,
		"empty id":       `{"kb_teams":[` + team("  ", "X") + `]}`,
		"empty name":     `{"kb_teams":[` + team("a", " ") + `]}`,
		"bad id":         `{"kb_teams":[` + team("a b", "X") + `]}`,
		"duplicate case": `{"kb_teams":[` + team("PAYMENTS", "X") + `,` + team("payments", "Y") + `]}`,
	}
	for name, in := range cases {
		if _, _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestParseUnknownTeamRows(t *testing.T) {
	d, st := mustParse(t, kb(`,
	 "kb_people":[{"team_id":"nope","name":"P"},{"team_id":" PayMents ","name":"Q"}],
	 "kb_reports":[{"team_id":"nope","report_name":"R"},{"team_id":" PayMents ","report_name":"R2"}],
	 "kb_team_projects":[{"team_id":"nope","title":"T"},{"team_id":" PayMents ","title":"T2"}],
	 "kb_past_work":[{"item_id":"1","owner_team":"","title":"W"},{"item_id":"2","owner_team":"nope"},
	   {"item_id":"3","owner_team":"a","involved_teams":"b; A ; zz; b","links":"https://x.test/a, b ; https://y.test"}]`))
	if len(d.People) != 1 || d.People[0].TeamID != "payments" || st.SkippedPeople != 1 {
		t.Fatalf("people %+v %+v", d.People, st)
	}
	if len(d.Reports) != 1 || d.Reports[0].TeamID != "payments" || st.SkippedReports != 1 {
		t.Fatalf("reports %+v", d.Reports)
	}
	if len(d.Projects) != 1 || d.Projects[0].TeamID != "payments" || st.SkippedProjects != 1 {
		t.Fatalf("projects %+v", d.Projects)
	}
	if len(d.PastWork) != 1 || st.SkippedPastWork != 2 {
		t.Fatalf("past work %+v %+v", d.PastWork, st)
	}
	w := d.PastWork[0]
	if !reflect.DeepEqual(w.Involved, []string{"b"}) || st.DroppedInvolved != 1 {
		t.Fatalf("involved %v dropped %d", w.Involved, st.DroppedInvolved)
	}
	// ';' splits only; the comma stays inside the token.
	if !reflect.DeepEqual(w.Links, []string{"https://x.test/a, b", "https://y.test"}) {
		t.Fatalf("links %q", w.Links)
	}
}

func TestParseIsLead(t *testing.T) {
	want := map[string]bool{"yes": true, "no": false, "Y": true, "N": false, "true": true, "false": false}
	for in, exp := range want {
		d, _ := mustParse(t, kb(`,"kb_people":[{"team_id":"a","name":"P","is_lead":"`+in+`"}]`))
		if d.People[0].IsLead != exp {
			t.Errorf("is_lead %q = %v, want %v", in, d.People[0].IsLead, exp)
		}
	}
}

func TestParseMultiValue(t *testing.T) {
	d, _ := mustParse(t, kb(""))
	var tm Team
	for _, x := range d.Teams {
		if x.ID == "a" {
			tm = x
		}
	}
	if !reflect.DeepEqual(tm.SlackChannels, []string{"#a", "#b"}) ||
		!reflect.DeepEqual(tm.JiraProjects, []string{"PAY", "OPS"}) ||
		!reflect.DeepEqual(tm.KeySystems, []string{"x, y", "z"}) ||
		len(tm.ConfluenceSpaces) != 0 {
		t.Fatalf("team %+v", tm)
	}
	if d.Teams[0].ID != "a" || d.Teams[2].ID != "payments" {
		t.Fatalf("teams not sorted: %v", d.Teams)
	}
}

func TestParseSentinelsAbsent(t *testing.T) {
	d, _ := mustParse(t, kb(`,
	 "kb_people":[{"team_id":"a","name":"P","slack_handle":"SENTINEL_HANDLE_7f3","hr_manager":"SENTINEL_HR_1","hr_title":"SENTINEL_TITLE"}],
	 "kb_directory":[{"name":"P","teams":"a","team_basis":"via manager (SENTINEL_MGR_9q1)","hr_manager":"SENTINEL_HR_2","source":"SENTINEL_SRC"}],
	 "kb_reports":[{"team_id":"a","report_name":"R","audience":"SENTINEL_AUD"}],
	 "kb_change_rules":[{"rule":"SENTINEL_HANDLE_7f3","n":3}]`))
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "SENTINEL") {
		t.Fatalf("sentinel leaked: %s", b)
	}
	if d.Directory[0].Basis != BasisManager {
		t.Fatalf("basis %q", d.Directory[0].Basis)
	}
}

func TestParseValidateRejectsMissingTeam(t *testing.T) {
	d, _ := mustParse(t, kb(""))
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	d.People = []Person{{TeamID: "ghost", Name: "P"}}
	if err := d.Validate(); err == nil {
		t.Fatal("Validate must reject unknown team")
	}
	if err := (&Directory{}).Validate(); err == nil {
		t.Fatal("Validate must reject zero teams")
	}
}

func TestParseDirectory(t *testing.T) {
	d, st := mustParse(t, kb(`,"kb_directory":[
	 {"name":"One","teams":" A ; b;a; zz","team_basis":"listed in KB","last_confirmed":"2026-10-21"},
	 {"name":"Two","teams":"zz","team_basis":"via department"},
	 {"name":"Three","teams":"a","team_basis":"whatever"},
	 {"name":"Four","teams":"a","team_basis":"via department"}]`))
	if len(d.Directory) != 3 || st.SkippedDirectoryRow != 1 || st.DroppedDirTeams != 2 {
		t.Fatalf("%+v %+v", d.Directory, st)
	}
	if !reflect.DeepEqual(d.Directory[0].Teams, []string{"a", "b"}) {
		t.Fatalf("teams %v", d.Directory[0].Teams)
	}
	got := []string{d.Directory[0].Basis, d.Directory[1].Basis, d.Directory[2].Basis}
	if !reflect.DeepEqual(got, []string{"listed", "other", "department"}) {
		t.Fatalf("basis %v", got)
	}
	if NormalizeBasis("via manager (Some Name)") != "manager" {
		t.Fatal("via manager")
	}
}
