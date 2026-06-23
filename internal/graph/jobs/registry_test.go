package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func TestRegistry_DefaultPoolSizes(t *testing.T) {
	r := jobs.NewRegistry()

	cases := []struct {
		typ       string
		wantPool  int
		wantLease time.Duration
	}{
		{"fetch_body", 8, 60 * time.Second},
		{"describe_attachment", 4, 120 * time.Second},
		{"resolve_identity", 4, 30 * time.Second},
		{"index_artifact", 4, 60 * time.Second},
		{"refresh_slack_groups", 1, 600 * time.Second},
		{"import_bamboohr", 1, 600 * time.Second},
		{"recompute_person_distance", 1, 600 * time.Second},
	}
	for _, tc := range cases {
		e, ok := r.Get(tc.typ)
		if !ok {
			t.Fatalf("registry missing type %q", tc.typ)
		}
		if e.PoolSize != tc.wantPool {
			t.Errorf("%s pool: got %d want %d", tc.typ, e.PoolSize, tc.wantPool)
		}
		if e.Lease != tc.wantLease {
			t.Errorf("%s lease: got %v want %v", tc.typ, e.Lease, tc.wantLease)
		}
	}
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := jobs.NewRegistry()
	r.Register("custom_job", jobs.Entry{
		PoolSize: 2,
		Lease:    45 * time.Second,
		Systems:  []string{"gemini"},
		Handler: func(ctx context.Context, payload []byte) error {
			return nil
		},
	})
	e, ok := r.Get("custom_job")
	if !ok || e.PoolSize != 2 {
		t.Fatalf("custom_job not registered")
	}
}
