package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// capRunes copies the exact behavior of handlers.capRunes (lead.go): cut to n
// runes including a trailing "…".
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func TestPseudonymizerUnchangedText(t *testing.T) {
	for _, in := range []string{
		"payment failed with timeout",
		"PAY-2420",
		"2026-10-12",
		"18:30",
		"see payments#2424 at 18:30 on 2026-10-12 about PAY-2420",
	} {
		if got := NewPseudonymizer().Apply(in); got != in {
			t.Errorf("Apply(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestPseudonymizerRefs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"booking reference AB1234", "booking reference ref-1"},
		{"https://x/y?id=AB12CD34EF56", "https://x/y?id=ref-1"},
		{"code AB12-CD34EF56GH end", "code ref-1 end"},
		{"AB1234 and AB1234 again: payment ref AB1234", "ref-1 and ref-1 again: payment ref ref-1"},
	}
	for _, c := range cases {
		if got := NewPseudonymizer().Apply(c.in); got != c.want {
			t.Errorf("Apply(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	p := NewPseudonymizer()
	if got := p.Apply("booking AB1234 then order CD5678 then booking ab1234"); got != "booking ref-1 then order ref-2 then booking ref-1" {
		t.Errorf("same ref twice / two refs: %q", got)
	}
	// A label followed by a no-digit word must not hide a later labelled ref.
	if got := NewPseudonymizer().Apply("ref payment AB1234"); got != "ref payment ref-1" {
		t.Errorf("label resume: %q", got)
	}
}

func TestPseudonymizerPeople(t *testing.T) {
	p := NewPseudonymizer()
	p.AddPerson("U1", "Ann Lee")
	got := p.Apply("<@U1> asked, @Ann Lee agreed, ann lee left")
	if got != "person-1 asked, person-1 agreed, person-1 left" {
		t.Errorf("registered person: %q", got)
	}
	if got := p.Apply("<@W9ZZ|bo> and <@U1>"); got != "person-2 and person-1" {
		t.Errorf("unregistered mention: %q", got)
	}
	// @Name that is not registered gets its own alias, stable across calls.
	if a, b := p.Apply("cc @Bob Tan"), p.Apply("@Bob Tan?"); !strings.Contains(a, "person-3") || !strings.Contains(b, "person-3") {
		t.Errorf("@name alias: %q / %q", a, b)
	}
	// Without AddPerson the mention and the name are not linked.
	q := NewPseudonymizer()
	if got := q.Apply("<@U1> and @Ann Lee"); got != "person-1 and person-2" {
		t.Errorf("unlinked: %q", got)
	}
	if strings.Contains(q.Apply("mail bob@Example.com"), "person") {
		t.Errorf("email must not be read as an @name")
	}
}

func TestPseudonymizerNamespaces(t *testing.T) {
	p := NewPseudonymizer()
	got := p.Apply("mail a.b@example.com and A.B@example.com, call +65 9123 4567, card 4111 1111 1111 1111")
	for _, want := range []string{"email-1 and email-1", "phone-1", "card-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "example.com") || strings.Contains(got, "9123") || strings.Contains(got, "4111") {
		t.Errorf("value leaked: %q", got)
	}
	// Same normalised value in two namespaces yields independent counters.
	q := NewPseudonymizer()
	if got := q.Apply("x@y.io booking AB1234"); got != "email-1 booking ref-1" {
		t.Errorf("separate namespaces: %q", got)
	}
}

func TestPseudonymizerUpstreamCutTail(t *testing.T) {
	full := strings.Repeat("word ", 118) + "AB12CD34EF56GH tail"
	cut := capRunes(full, 600)
	if !strings.HasSuffix(cut, "…") {
		t.Fatalf("fixture not cut: %q", cut[len(cut)-30:])
	}
	if utf8.RuneCountInString(cut) != 600 {
		t.Fatalf("fixture length %d", utf8.RuneCountInString(cut))
	}
	// pick a cut that leaves a 5-char fragment of the 14-char ref.
	frag := strings.Repeat("word ", 118) + "AB12C…"
	if got := NewPseudonymizer().Apply(frag); !strings.HasSuffix(got, "ref-1…") || strings.Contains(got, "AB12C") {
		t.Errorf("tail fragment not aliased: %q", got[len(got)-20:])
	}
	// the real cut: build so the 600-rune boundary falls inside the ref.
	prefix := strings.Repeat("w", 600-1-5-1) + " " // 594 runes, then 5 ref chars, then the cap's "…"
	real := capRunes(prefix+"AB12CD34EF56GH", 600)
	if got := NewPseudonymizer().Apply(real); strings.Contains(got, "AB12C") || !strings.Contains(got, "ref-") {
		t.Errorf("real capRunes cut: %q", got[len(got)-20:])
	}
	// no digit in the tail token: left alone.
	if got := NewPseudonymizer().Apply("some prose cut…"); got != "some prose cut…" {
		t.Errorf("digitless tail changed: %q", got)
	}
}

func TestPseudonymizerNoValueLeaksAndEmpty(t *testing.T) {
	p := NewPseudonymizer()
	if p.Apply("") != "" {
		t.Fatal("empty")
	}
	if got := p.Apply("odd \uE000a\uE001 input"); strings.ContainsAny(got, "\uE000\uE001") {
		t.Errorf("shield chars survive: %q", got)
	}
}

// code-r1 owner decision: a label word beats every shield.
func TestPseudonymizerLabelBeatsShields(t *testing.T) {
	cases := []struct{ in, want string }{
		{"booking BKG-1234567890 failed", "booking ref-1 failed"},
		{"pnr XY-123456789012", "pnr ref-1"},
		{"order#123456789 failed", "order#ref-1 failed"},
		{"ref 8812-34-5678901", "ref ref-1"},
		{"payment PAY-2420", "payment ref-1"},
		{"ref 2026-10-12", "ref ref-1"},
		{"txn 18:30", "txn ref-1"},
		{"booking repo#1234", "booking ref-1"},
	}
	for _, c := range cases {
		if got := NewPseudonymizer().Apply(c.in); got != c.want {
			t.Errorf("Apply(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPseudonymizerShieldBoundaries(t *testing.T) {
	// Long Jira-like keys and dates/times inside longer digit runs are not shielded.
	for in, want := range map[string]string{
		"see BKG-1234567890 now":     "see ref-1 now",
		"at 1218:305 x":              "at 1218:305 x",
		"stamp 12345-67-890123456 x": "stamp ref-1 x",
	} {
		if got := NewPseudonymizer().Apply(in); got != want {
			t.Errorf("Apply(%q) = %q, want %q", in, got, want)
		}
	}
	// Shielded date inside a longer digit run would hide an 8+ digit ref.
	if got := NewPseudonymizer().Apply("n 20261012345 x"); got != "n ref-1 x" {
		t.Errorf("digit run: %q", got)
	}
}

func TestPseudonymizerLongLabelledToken(t *testing.T) {
	tok := "AB12" + strings.Repeat("C", 36) + "90XYZ12"
	got := NewPseudonymizer().Apply("ref " + tok + " end")
	if got != "ref ref-1 end" {
		t.Errorf("long token: %q", got)
	}
}

func TestPseudonymizerUnicodeNames(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"محمد علي", "قال محمد علي إنه", "قال person-1 إنه"},
		{"Zoë", "Zoë said", "person-1 said"},
		{"Zoë", "ask zoë.", "ask person-1."},
		{"Ann (Ops)", "Ann said hi", "person-1 said hi"},
		{"Ann (Ops)", "Ann (Ops) said hi", "person-1 said hi"},
		{"Ann Lee (OOO)", "plain Ann Lee here", "plain person-1 here"},
		{"J.D.", "J.D. said", "person-1 said"},
		{"J.D.", "told @J.D. ok", "told person-1 ok"},
	}
	for _, c := range cases {
		p := NewPseudonymizer()
		p.AddPerson("U1", c.name)
		if got := p.Apply(c.in); got != c.want {
			t.Errorf("name %q: Apply(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	// Edges stay whole-word: no match inside a longer word.
	p := NewPseudonymizer()
	p.AddPerson("U1", "Zoë")
	if got := p.Apply("Zoëy and xZoë"); got != "Zoëy and xZoë" {
		t.Errorf("inside word: %q", got)
	}
	// Adjacent registered names are both replaced.
	p = NewPseudonymizer()
	p.AddPerson("U1", "Ann Lee")
	if got := p.Apply("Ann Lee Ann Lee"); got != "person-1 person-1" {
		t.Errorf("adjacent: %q", got)
	}
}

func TestPseudonymizerShieldPlaceholderNotMatched(t *testing.T) {
	p := NewPseudonymizer()
	p.AddPerson("U1", "E")
	var in []string
	for i := 0; i < 50; i++ {
		in = append(in, "PAY-5")
	}
	got := p.Apply(strings.Join(in, " "))
	if got != strings.Join(in, " ") {
		t.Errorf("placeholder leaked or matched: %q", got)
	}
}

func TestPseudonymizerRepeatedRefAliasInShieldShapes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"booking BKG-123456 failed; BKG-123456 retried", "booking ref-1 failed; ref-1 retried"},
		{"payment PAY-2420 see PAY-2420", "payment ref-1 see ref-1"},
		{"ref 2026-10-12 and later 2026-10-12", "ref ref-1 and later ref-1"},
		{"ref 18:30 then 18:30", "ref ref-1 then ref-1"},
		{"ref payments#2424 and payments#2424", "ref ref-1 and ref-1"},
	} {
		if got := NewPseudonymizer().Apply(c.in); got != c.want {
			t.Errorf("Apply(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Cross-row: one Pseudonymizer, alias known from an earlier row.
	p := NewPseudonymizer()
	if got := p.Apply("booking BKG-123456"); got != "booking ref-1" {
		t.Fatalf("row 2: %q", got)
	}
	if got := p.Apply("BKG-123456 again"); got != "ref-1 again" {
		t.Errorf("row 3: %q", got)
	}
}

func TestPseudonymizerShieldBoundariesAdjacent(t *testing.T) {
	for _, in := range []string{
		"2026-10-12,2026-10-13", "2026-10-12 2026-10-13", "18:30,19:45", "18:30:05;19:45:10",
	} {
		if got := NewPseudonymizer().Apply(in); got != in {
			t.Errorf("Apply(%q) = %q, want unchanged", in, got)
		}
	}
	if got := NewPseudonymizer().Apply("ref 12:3456789 x"); got != "ref ref-1 x" {
		t.Errorf("labelled time-prefixed run: %q", got)
	}
}
