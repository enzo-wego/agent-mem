package hooks

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestInstallOmOUsesDiscoveredExtensionDestination(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	omo, senpi, pi := filepath.Join(home, "omo"), filepath.Join(home, "senpi"), filepath.Join(home, "pi")
	defaultAgent := filepath.Join(home, ".omo", "agent")

	for _, tt := range []struct {
		name           string
		omo, senpi, pi string
		opts           InstallOptions
		wantPath       string
	}{
		{name: "default user", wantPath: filepath.Join(defaultAgent, "extensions", "agent-mem.ts")},
		{name: "branded user", omo: omo, senpi: senpi, pi: pi, wantPath: filepath.Join(omo, "extensions", "agent-mem.ts")},
		{name: "senpi user", omo: " ", senpi: senpi, pi: pi, wantPath: filepath.Join(senpi, "extensions", "agent-mem.ts")},
		{name: "legacy pi user", pi: pi, wantPath: filepath.Join(pi, "extensions", "agent-mem.ts")},
		{
			name: "project", omo: omo, opts: InstallOptions{Scope: "project", ProjectDir: project},
			wantPath: filepath.Join(project, ".omo", "extensions", "agent-mem.ts"),
		},
		{
			name: "explicit beats project", omo: omo, opts: InstallOptions{
				Scope: "project", ProjectDir: project, HooksPath: filepath.Join(project, "custom", "memory.ts"),
			},
			wantPath: filepath.Join(project, "custom", "memory.ts"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OMO_CODING_AGENT_DIR", tt.omo)
			t.Setenv("SENPI_CODING_AGENT_DIR", tt.senpi)
			t.Setenv("PI_CODING_AGENT_DIR", tt.pi)

			result, err := InstallHooksWithOptions(" OmO ", tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.Path != tt.wantPath || !result.Changed || !result.Created {
				t.Fatalf("first install = %+v, want new file at %s", result, tt.wantPath)
			}
			assertOmOExtensionBytes(t, tt.wantPath)
		})
	}
}

func TestInstallOmOUpdatesOnlyManagedExtensionAndThenBecomesNoOp(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent-mem.ts")
	neighbor := filepath.Join(dir, "user-extension.ts")
	unrelated := []byte("unrelated user extension")
	for path, data := range map[string][]byte{dest: []byte("previous agent-mem release"), neighbor: unrelated} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	updated, err := InstallHooks("omo", dest)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Created || !updated.Changed {
		t.Fatalf("upgrade = %+v, want changed but not created", updated)
	}
	wantEvents := []string{"session_start", "input", "input_disposition", "tool_result", "agent_end", "agent_settled"}
	if !slices.Equal(updated.ChangedEvents, wantEvents) {
		t.Fatalf("changed events = %v, want %v", updated.ChangedEvents, wantEvents)
	}
	assertOmOExtensionBytes(t, dest)
	if got, err := os.ReadFile(neighbor); err != nil || !bytes.Equal(got, unrelated) {
		t.Fatalf("neighbor = %q, error = %v", got, err)
	}

	repeated, err := InstallHooks("omo", dest)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Created || repeated.Changed || len(repeated.ChangedEvents) != 0 {
		t.Fatalf("reinstall = %+v, want no changes", repeated)
	}
}

func assertOmOExtensionBytes(t *testing.T, dest string) {
	t.Helper()
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("assets/omo/agent-mem.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("installed %s does not match the shipped OmO extension", dest)
	}
}
