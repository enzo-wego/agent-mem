package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/config"
)

const filteredMsg = "Project not recorded: not allowed by allowed_projects/ignored_projects"

func newFilterServer(allowed, ignored string) *Server {
	return &Server{config: &config.Config{AllowedProjects: allowed, IgnoredProjects: ignored}}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&buf))
	t.Cleanup(func() { log.Logger = old })
	return &buf
}

// filteredEvents returns the project of every new-message event, asserting level.
func filteredEvents(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var projects []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if ev["message"] != filteredMsg {
			continue
		}
		if ev["level"] != "info" {
			t.Errorf("level = %v, want info", ev["level"])
		}
		p, _ := ev["project"].(string)
		projects = append(projects, p)
	}
	return projects
}

func TestProjectAllowedTable(t *testing.T) {
	cases := []struct {
		allowed, ignored, project string
		want                      bool
	}{
		{"a,b", "", "a", true},
		{"a,b", "", "x", false},
		{"", "x", "x", false},
		{"", "x", "a", true},
		{"", "", "a", true},
		{"a,b", "a", "a", true},
		{"A, b ,,", "", "a", true},
		{"a", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		s := newFilterServer(c.allowed, c.ignored)
		if got := s.isProjectAllowed(c.project); got != c.want {
			t.Errorf("allowed=%q ignored=%q project=%q: got %v want %v", c.allowed, c.ignored, c.project, got, c.want)
		}
	}
}

func TestProjectFilteredLoggedOnce(t *testing.T) {
	for name, s := range map[string]*Server{
		"whitelist miss": newFilterServer("a", ""),
		"blacklist hit":  newFilterServer("", "x"),
	} {
		t.Run(name, func(t *testing.T) {
			buf := captureLogs(t)
			for range 3 {
				s.isProjectAllowed("x")
			}
			got := filteredEvents(t, buf)
			if len(got) != 1 || got[0] != "x" {
				t.Fatalf("events = %v, want [x]", got)
			}
		})
	}
}

func TestProjectDistinctEachLogged(t *testing.T) {
	buf := captureLogs(t)
	s := newFilterServer("a", "")
	for _, p := range []string{"x", "y", "x", "y", "z"} {
		s.isProjectAllowed(p)
	}
	if got := filteredEvents(t, buf); len(got) != 3 {
		t.Fatalf("events = %v, want 3", got)
	}
}

func TestProjectConcurrentOncePerProject(t *testing.T) {
	buf := captureLogs(t)
	s := newFilterServer("a", "")
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(2)
		go func() { defer wg.Done(); s.isProjectAllowed("same") }()
		go func(i int) { defer wg.Done(); s.isProjectAllowed(fmt.Sprintf("p%d", i%5)) }(i)
	}
	wg.Wait()
	counts := map[string]int{}
	for _, p := range filteredEvents(t, buf) {
		counts[p]++
	}
	if len(counts) != 6 {
		t.Fatalf("counts = %v, want 6 projects", counts)
	}
	for p, n := range counts {
		if n != 1 {
			t.Errorf("project %s logged %d times", p, n)
		}
	}
}

func TestProjectPerServerSet(t *testing.T) {
	buf := captureLogs(t)
	newFilterServer("a", "").isProjectAllowed("x")
	newFilterServer("a", "").isProjectAllowed("x")
	if got := filteredEvents(t, buf); len(got) != 2 {
		t.Fatalf("events = %v, want 2", got)
	}
}

func TestProjectRejectAllowRejectLogsOnce(t *testing.T) {
	buf := captureLogs(t)
	s := newFilterServer("a", "")
	s.isProjectAllowed("x")
	s.config.AllowedProjects = "x"
	if !s.isProjectAllowed("x") {
		t.Fatal("want allowed")
	}
	s.config.AllowedProjects = "a"
	s.isProjectAllowed("x")
	if got := filteredEvents(t, buf); len(got) != 1 {
		t.Fatalf("events = %v, want 1", got)
	}
}

func TestProjectAllowedAndEmptyLogNothing(t *testing.T) {
	buf := captureLogs(t)
	s := newFilterServer("a", "")
	s.isProjectAllowed("a")
	s.isProjectAllowed("")
	newFilterServer("", "").isProjectAllowed("q")
	if got := filteredEvents(t, buf); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}
