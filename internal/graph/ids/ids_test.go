package ids

import (
	"testing"
)

func TestSlackThread(t *testing.T) {
	t.Parallel()
	got := SlackThread("C08S954G2LX", "1779709917.613979")
	want := "slack:C08S954G2LX:1779709917.613979"
	if got != want {
		t.Fatalf("SlackThread = %q, want %q", got, want)
	}
}

func TestSlackMessage(t *testing.T) {
	t.Parallel()
	got := SlackMessage("C08S954G2LX", "1779709917.613979")
	want := "slack:C08S954G2LX:1779709917.613979"
	if got != want {
		t.Fatalf("SlackMessage = %q, want %q", got, want)
	}
}

func TestSlackFile(t *testing.T) {
	t.Parallel()
	got := SlackFile("F0B5T0WD39P")
	want := "slack_file:F0B5T0WD39P"
	if got != want {
		t.Fatalf("SlackFile = %q, want %q", got, want)
	}
}

func TestJira(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "valid", input: "PAY-2128", want: "jira:PAY-2128"},
		{name: "valid multi-char project", input: "WEGO-1", want: "jira:WEGO-1"},
		{name: "lowercase rejected", input: "pay-2128", wantErr: true},
		{name: "no digits", input: "PAY-", wantErr: true},
		{name: "plain word", input: "not-a-key", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Jira(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Jira(%q): expected error, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("Jira(%q): unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("Jira(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGHPR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		repo    string
		number  int
		want    string
		wantErr bool
	}{
		{name: "valid", repo: "wego/payments", number: 1960, want: "gh_pr:wego/payments#1960"},
		{name: "zero number", repo: "wego/payments", number: 0, wantErr: true},
		{name: "negative number", repo: "wego/payments", number: -1, wantErr: true},
		{name: "bad repo no slash", repo: "wegopayments", number: 1, wantErr: true},
		{name: "empty repo", repo: "", number: 1, wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := GHPR(tt.repo, tt.number)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("GHPR(%q, %d): expected error, got nil", tt.repo, tt.number)
				}
				return
			}
			if err != nil {
				t.Fatalf("GHPR(%q, %d): unexpected error: %v", tt.repo, tt.number, err)
			}
			if got != tt.want {
				t.Fatalf("GHPR(%q, %d) = %q, want %q", tt.repo, tt.number, got, tt.want)
			}
		})
	}
}

func TestCFPage(t *testing.T) {
	t.Parallel()
	got := CFPage(3861872666)
	want := "cf:3861872666"
	if got != want {
		t.Fatalf("CFPage = %q, want %q", got, want)
	}
}

func TestPagerDuty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "valid", input: "Q03YWZHA1OXQNR", want: "pagerduty:Q03YWZHA1OXQNR"},
		{name: "empty", input: "", wantErr: true},
		{name: "contains hyphen", input: "Q03-ABC", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := PagerDuty(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("PagerDuty(%q): expected error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("PagerDuty(%q): unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("PagerDuty(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDatadog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		objectType string
		id         int64
		want       string
		wantErr    bool
	}{
		{name: "monitor", objectType: "monitor", id: 133274814, want: "datadog:monitor:133274814"},
		{name: "dashboard", objectType: "dashboard", id: 42, want: "datadog:dashboard:42"},
		{name: "log", objectType: "log", id: 7, want: "datadog:log:7"},
		{name: "unknown type", objectType: "metric", id: 1, wantErr: true},
		{name: "empty type", objectType: "", id: 1, wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Datadog(tt.objectType, tt.id)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Datadog(%q, %d): expected error", tt.objectType, tt.id)
				}
				return
			}
			if err != nil {
				t.Fatalf("Datadog(%q, %d): unexpected error: %v", tt.objectType, tt.id, err)
			}
			if got != tt.want {
				t.Fatalf("Datadog(%q, %d) = %q, want %q", tt.objectType, tt.id, got, tt.want)
			}
		})
	}
}

func TestSentry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "valid", input: "WEGO-PAYMENTS-1PJD", want: "sentry:WEGO-PAYMENTS-1PJD"},
		{name: "empty", input: "", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Sentry(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Sentry(%q): expected error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("Sentry(%q): unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("Sentry(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGWSDoc(t *testing.T) {
	t.Parallel()
	got := GWSDoc("1abcXYZ")
	want := "gws_doc:1abcXYZ"
	if got != want {
		t.Fatalf("GWSDoc = %q, want %q", got, want)
	}
}

func TestPartner(t *testing.T) {
	t.Parallel()
	tests := []struct{ input, want string }{
		{"TripleA", "partner:triplea"},
		{"auto refund", "partner:auto-refund"},
		{"checkout", "partner:checkout"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := Partner(tt.input)
			if got != tt.want {
				t.Fatalf("Partner(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFeature(t *testing.T) {
	t.Parallel()
	tests := []struct{ input, want string }{
		{"auto refund", "feature:auto_refund"},
		{"AutoRefund", "feature:autorefund"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := Feature(tt.input)
			if got != tt.want {
				t.Fatalf("Feature(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()
	got := Status("Pending")
	want := "status:pending"
	if got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}
}

func TestCurrency(t *testing.T) {
	t.Parallel()
	got := Currency("TRY")
	want := "currency:try"
	if got != want {
		t.Fatalf("Currency = %q, want %q", got, want)
	}
}

func TestCodeFile(t *testing.T) {
	t.Parallel()
	got := CodeFile("pkg/payment/tabby/integration.go")
	want := "code_file:pkg/payment/tabby/integration.go"
	if got != want {
		t.Fatalf("CodeFile = %q, want %q", got, want)
	}
}

func TestPerson(t *testing.T) {
	t.Parallel()
	got := Person("Lei@Wego.com")
	want := "person:lei@wego.com"
	if got != want {
		t.Fatalf("Person = %q, want %q", got, want)
	}
}

func TestUserGroup(t *testing.T) {
	t.Parallel()
	got := UserGroup("S01TMG8Q65R")
	want := "usergroup:S01TMG8Q65R"
	if got != want {
		t.Fatalf("UserGroup = %q, want %q", got, want)
	}
}

func TestParseType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		nodeID  string
		want    NodeType
		wantOK  bool
	}{
		{"slack:C08S954G2LX:1779709917.613979", TypeSlackThread, true},
		{"jira:PAY-2128", TypeJira, true},
		{"gh_pr:wego/payments#1960", TypeGHPR, true},
		{"cf:3861872666", TypeCFPage, true},
		{"pagerduty:Q03YWZHA1OXQNR", TypePagerDuty, true},
		{"datadog:monitor:133274814", TypeDatadog, true},
		{"sentry:WEGO-PAYMENTS-1PJD", TypeSentry, true},
		{"gws_doc:1abcXYZ", TypeGWSDoc, true},
		{"slack_file:F0B5T0WD39P", TypeSlackFile, true},
		{"partner:triplea", TypePartner, true},
		{"feature:auto_refund", TypeFeature, true},
		{"status:none", TypeStatus, true},
		{"currency:try", TypeCurrency, true},
		{"code_file:pkg/payment/tabby/integration.go", TypeCodeFile, true},
		{"person:lei@wego.com", TypePerson, true},
		{"usergroup:S01TMG8Q65R", TypeUserGroup, true},
		// malformed
		{"nocolon", "", false},
		{"", "", false},
		{"unknown:foo", "", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.nodeID, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseType(tt.nodeID)
			if ok != tt.wantOK {
				t.Fatalf("ParseType(%q) ok=%v, want %v", tt.nodeID, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("ParseType(%q) = %q, want %q", tt.nodeID, got, tt.want)
			}
		})
	}
}

func TestParseNaturalKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		nodeID  string
		want    string
		wantOK  bool
	}{
		{"jira:PAY-2128", "PAY-2128", true},
		{"datadog:monitor:133274814", "monitor:133274814", true},
		{"slack:C08S954G2LX:1779709917.613979", "C08S954G2LX:1779709917.613979", true},
		{"gh_pr:wego/payments#1960", "wego/payments#1960", true},
		{"nocolon", "", false},
		{"prefix:", "", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.nodeID, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseNaturalKey(tt.nodeID)
			if ok != tt.wantOK {
				t.Fatalf("ParseNaturalKey(%q) ok=%v, want %v", tt.nodeID, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("ParseNaturalKey(%q) = %q, want %q", tt.nodeID, got, tt.want)
			}
		})
	}
}
