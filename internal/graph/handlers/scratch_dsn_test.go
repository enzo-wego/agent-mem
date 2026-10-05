package handlers

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCheckScratchDSN(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{"scratch", "postgres://scratch:scratch@127.0.0.1:1/agentmem_test?sslmode=disable", false},
		{"live", "postgres://scratch:scratch@127.0.0.1:1/agentmem?sslmode=disable", true},
		{"postgres", "postgres://scratch:scratch@127.0.0.1:1/postgres?sslmode=disable", true},
		{"malformed", "postgres://%zz", true},
		{"query_override", "postgres://scratch:scratch@127.0.0.1:1/agentmem_test?dbname=agentmem&sslmode=disable", true},
		{"keyword_live", "host=127.0.0.1 port=1 user=scratch password=scratch dbname=agentmem sslmode=disable", true},
		{"keyword_scratch", "host=127.0.0.1 port=1 user=scratch password=scratch dbname=agentmem_test sslmode=disable", false},
		{"other_test_database", "postgres://scratch:scratch@127.0.0.1:1/other_test?sslmode=disable", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkScratchDSN(tc.dsn)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkScratchDSN error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "refusing to run:") {
				t.Fatalf("missing refusal message: %v", err)
			}
		})
	}
}

// Invoke the real consumer in a subprocess so its Fatal can be inspected.
// Port 1 is deliberately not a database: refusal must precede any connection.
func TestScratchDBRefusesBeforeConnect(t *testing.T) {
	if consumer := os.Getenv("AGENT_MEM_SCRATCH_GUARD_CONSUMER"); consumer != "" {
		switch consumer {
		case "openTestDB":
			openTestDB(t)
		case "periodicHandlerDB":
			periodicHandlerDB(t)
		case "eligibilityLookupErrorPool":
			eligibilityLookupErrorPool(t, 0)
		default:
			t.Fatalf("unknown scratch guard consumer %q", consumer)
		}
		t.Fatal("unsafe database was accepted")
	}
	for _, consumer := range []string{"openTestDB", "periodicHandlerDB", "eligibilityLookupErrorPool"} {
		for _, dsn := range []string{
			"postgres://scratch:scratch@127.0.0.1:1/postgres?sslmode=disable",
			"postgres://scratch:scratch@127.0.0.1:1/agentmem_test?dbname=agentmem&sslmode=disable",
			"postgres://%zz",
		} {
			t.Run(consumer+"/"+dsn, func(t *testing.T) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestScratchDBRefusesBeforeConnect$", "-test.timeout=10s")
				for _, entry := range os.Environ() {
					if !strings.HasPrefix(entry, "DATABASE_URL=") && !strings.HasPrefix(entry, "AGENT_MEM_SCRATCH_GUARD_CONSUMER=") {
						cmd.Env = append(cmd.Env, entry)
					}
				}
				cmd.Env = append(cmd.Env, "DATABASE_URL="+dsn, "AGENT_MEM_SCRATCH_GUARD_CONSUMER="+consumer)
				out, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(out), "refusing to run:") {
					t.Fatalf("consumer did not refuse before connecting: err=%v\n%s", err, out)
				}
			})
		}
	}
}
