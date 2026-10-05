package handlers

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func TestRegisterAll_HeartbeatJobsHaveMaxRuntime(t *testing.T) {
	// Match worker boot: RegisterAll replaces NewRegistry's placeholder entries.
	reg := jobs.NewRegistry()
	RegisterAll(reg, Deps{})

	types := reg.Types()
	sort.Strings(types)
	var heartbeatTypes []string
	for _, typ := range types {
		entry, ok := reg.Get(typ)
		if !ok {
			t.Fatalf("registered job %q not found", typ)
		}
		if !entry.Heartbeat {
			continue
		}
		heartbeatTypes = append(heartbeatTypes, typ)
		if entry.MaxRuntime <= 0 {
			t.Errorf("heartbeat job %q has no runtime ceiling: %v", typ, entry.MaxRuntime)
		}
	}
	want := []string{
		"backfill_identifiers",
		"recompute_person_distance",
		"refresh_slack_members",
		"refresh_topic_scope",
	}
	if !reflect.DeepEqual(heartbeatTypes, want) {
		t.Errorf("final heartbeat types = %v, want %v", heartbeatTypes, want)
	}
	t.Logf("final heartbeat types: %v", heartbeatTypes)
}

func TestRegisterAll_ReRegisteredJobsKeepLeaseTimeouts(t *testing.T) {
	reg := jobs.NewRegistry()
	RegisterAll(reg, Deps{})
	for _, tc := range []struct {
		typ       string
		wantLease time.Duration
	}{
		{typ: "refresh_slack_groups", wantLease: 5 * time.Minute},
		{typ: "import_bamboohr", wantLease: 10 * time.Minute},
	} {
		entry, ok := reg.Get(tc.typ)
		if !ok {
			t.Fatalf("registry missing job %q", tc.typ)
		}
		if entry.Heartbeat || entry.MaxRuntime != 0 || entry.Lease != tc.wantLease {
			t.Errorf("%s: heartbeat=%v MaxRuntime=%v lease=%v, want false, zero, %v",
				tc.typ, entry.Heartbeat, entry.MaxRuntime, entry.Lease, tc.wantLease)
		}
	}
}
