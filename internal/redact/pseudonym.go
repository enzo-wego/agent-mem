package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Pseudonymizer replaces identifying values in the text of ONE case with stable
// aliases (person-1, ref-2, ...), so a reader can still tell whether two
// references are the same without seeing the value. The alias map lives in
// memory only and is never written or logged. Not safe for concurrent use.
type Pseudonymizer struct {
	next    map[string]int
	alias   map[string]string // "<ns>|<key>" -> "<ns>-<n>"
	persons []personName
	nameRes []nameRe // compiled lazily from persons
	shields []string // per Apply call
}

type personName struct{ name, alias string }

type nameRe struct {
	re    *regexp.Regexp
	alias string
}

// NewPseudonymizer returns an empty per-case pseudonymizer.
func NewPseudonymizer() *Pseudonymizer {
	return &Pseudonymizer{next: map[string]int{}, alias: map[string]string{}}
}

const (
	shieldOpen  = "\uE000"
	shieldClose = "\uE001"
)

var (
	reShieldTok  = regexp.MustCompile("\uE000([a-j]+)\uE001")
	reShieldJira = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)
	reShieldHash = regexp.MustCompile(`[A-Za-z0-9_.-]+#\d+`)
	reShieldDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	reShieldTime = regexp.MustCompile(`\d{2}:\d{2}(?::\d{2})?`)

	reMention = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)
	// group 1 is the preceding boundary, group 2 the @-name.
	reAtName = regexp.MustCompile(`(^|[^\p{L}\p{N}._%+-])(@\p{Lu}[\p{L}'’-]*(?:\s+\p{Lu}[\p{L}'’-]*){0,2})`)
	// group 1 is the label word, group 2 the token.
	reLabelledRef = regexp.MustCompile(`(?i)\b(booking|payment|order|pnr|bid|txn|transaction|ref|reference)\b(?:\s+(?:ref|reference|id|no|number)\b)?\.?\s*[:#=]?\s*([A-Za-z0-9][A-Za-z0-9-]{3,39})`)
	reRun         = regexp.MustCompile(`[A-Za-z0-9_-]+`)
	reCutTail     = regexp.MustCompile(`([A-Za-z0-9-]+)…$`)
)

func hasDigitStr(s string) bool {
	for i := range s {
		if isDigit(s[i]) {
			return true
		}
	}
	return false
}

func normAlias(v string) string {
	v = strings.ToLower(v)
	return strings.NewReplacer("-", "", " ", "").Replace(v)
}

// aliasFor returns the alias of key in namespace ns, creating it on first use.
func (p *Pseudonymizer) aliasFor(ns, key string) string {
	k := ns + "|" + key
	if a, ok := p.alias[k]; ok {
		return a
	}
	p.next[ns]++
	a := fmt.Sprintf("%s-%d", ns, p.next[ns])
	p.alias[k] = a
	return a
}

// AddPerson links a Slack user id and a display name (either may be empty) to
// one person alias, so <@id>, @Name and Name map to the same person-N.
func (p *Pseudonymizer) AddPerson(slackID, displayName string) {
	idKey, nameKey := "", normAlias(strings.TrimSpace(displayName))
	if slackID != "" {
		idKey = "id:" + strings.ToLower(slackID)
	}
	if idKey == "" && nameKey == "" {
		return
	}
	var a string
	switch {
	case idKey != "" && p.alias["person|"+idKey] != "":
		a = p.alias["person|"+idKey]
	case nameKey != "" && p.alias["person|"+nameKey] != "":
		a = p.alias["person|"+nameKey]
	default:
		a = p.aliasFor("person", firstNonEmptyStr(idKey, nameKey))
	}
	if idKey != "" {
		p.alias["person|"+idKey] = a
	}
	if nameKey != "" {
		p.alias["person|"+nameKey] = a
		p.persons = append(p.persons, personName{name: strings.TrimSpace(displayName), alias: a})
		p.nameRes = nil
	}
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (p *Pseudonymizer) shield(lit string) string {
	idx := len(p.shields)
	p.shields = append(p.shields, lit)
	var b strings.Builder
	b.WriteString(shieldOpen)
	if idx == 0 {
		b.WriteByte('a')
	}
	var digits []byte
	for n := idx; n > 0; n /= 10 {
		digits = append(digits, byte('a'+n%10))
	}
	for i := len(digits) - 1; i >= 0; i-- {
		b.WriteByte(digits[i])
	}
	b.WriteString(shieldClose)
	return b.String()
}

func (p *Pseudonymizer) unshield(s string) string {
	return reShieldTok.ReplaceAllStringFunc(s, func(m string) string {
		t := m[len(shieldOpen) : len(m)-len(shieldClose)]
		idx := 0
		for i := range t {
			idx = idx*10 + int(t[i]-'a')
		}
		if idx < len(p.shields) {
			return p.shields[idx]
		}
		return m
	})
}

// replaceGroup replaces capture group g of every match with fn(group text).
func replaceGroup(re *regexp.Regexp, s string, g int, fn func(string) string) string {
	locs := re.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range locs {
		if m[2*g] < 0 {
			continue
		}
		b.WriteString(s[last:m[2*g]])
		b.WriteString(fn(s[m[2*g]:m[2*g+1]]))
		last = m[2*g+1]
	}
	b.WriteString(s[last:])
	return b.String()
}

func (p *Pseudonymizer) compileNames() {
	if p.nameRes != nil || len(p.persons) == 0 {
		return
	}
	ps := append([]personName(nil), p.persons...)
	sort.SliceStable(ps, func(i, j int) bool { return len(ps[i].name) > len(ps[j].name) })
	for _, pn := range ps {
		words := strings.Fields(pn.name)
		for i := range words {
			words[i] = regexp.QuoteMeta(words[i])
		}
		re, err := regexp.Compile(`(?i)@?\b` + strings.Join(words, `\s+`) + `\b`)
		if err != nil {
			continue
		}
		p.nameRes = append(p.nameRes, nameRe{re: re, alias: pn.alias})
	}
}

// Apply pseudonymizes s: protected tokens are shielded, then mentions, names,
// emails/phones/cards and reference-like tokens are replaced by aliases, and
// the result goes through Mask as a second pass.
func (p *Pseudonymizer) Apply(s string) string {
	if s == "" {
		return s
	}
	p.shields = p.shields[:0]
	s = strings.Map(func(r rune) rune {
		if r == '\uE000' || r == '\uE001' {
			return '?'
		}
		return r
	}, s)

	// 1. shield protected tokens.
	for _, re := range []*regexp.Regexp{reShieldJira, reShieldHash, reShieldDate, reShieldTime} {
		s = re.ReplaceAllStringFunc(s, p.shield)
	}
	// 2. Slack mentions.
	s = reMention.ReplaceAllStringFunc(s, func(m string) string {
		id := reMention.FindStringSubmatch(m)[1]
		return p.shield(p.aliasFor("person", "id:"+strings.ToLower(id)))
	})
	// 3. registered display names.
	p.compileNames()
	for _, nr := range p.nameRes {
		alias := nr.alias
		s = nr.re.ReplaceAllStringFunc(s, func(string) string { return p.shield(alias) })
	}
	// 4. other @Name.
	s = replaceGroup(reAtName, s, 2, func(m string) string {
		return p.shield(p.aliasFor("person", normAlias(strings.TrimPrefix(m, "@"))))
	})
	// 5. emails, phones, cards.
	s = maskFunc(s, func(kind, text string) string {
		ns := strings.Trim(kind, "[]")
		return p.shield(p.aliasFor(ns, normAlias(text)))
	})
	// 6. labelled refs; a token without a digit is not a ref.
	s = p.labelledRefs(s)
	// 7. unlabelled refs.
	s = reRun.ReplaceAllStringFunc(s, func(tok string) string {
		if refLike(tok) {
			return p.shield(p.aliasFor("ref", normAlias(tok)))
		}
		return tok
	})
	// 8. a ref cut by an upstream length cap.
	if strings.HasSuffix(s, "…") {
		s = replaceGroup(reCutTail, s, 1, func(tok string) string {
			if hasDigitStr(tok) {
				return p.shield(p.aliasFor("ref", normAlias(tok)))
			}
			return tok
		})
	}
	return Mask(p.unshield(s))
}

func (p *Pseudonymizer) labelledRefs(s string) string {
	var b strings.Builder
	pos := 0
	for pos < len(s) {
		m := reLabelledRef.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		labelEnd, t0, t1 := pos+m[3], pos+m[4], pos+m[5]
		if !hasDigitStr(s[t0:t1]) {
			// Not a ref: resume right after the label so the next label can match.
			b.WriteString(s[pos:labelEnd])
			pos = labelEnd
			continue
		}
		b.WriteString(s[pos:t0])
		b.WriteString(p.shield(p.aliasFor("ref", normAlias(s[t0:t1]))))
		pos = t1
	}
	b.WriteString(s[pos:])
	return b.String()
}

// refLike reports whether a maximal [A-Za-z0-9_-] run is an unlabelled ref:
// with hyphens removed, 10+ chars mixing letters and digits, or 8+ digits.
func refLike(tok string) bool {
	t := strings.ReplaceAll(tok, "-", "")
	letters, digits, other := 0, 0, 0
	for _, r := range t {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case unicode.IsLetter(r):
			letters++
		default:
			other++
		}
	}
	n := letters + digits + other
	if letters == 0 && other == 0 && digits >= 8 {
		return true
	}
	return n >= 10 && letters > 0 && digits > 0
}
