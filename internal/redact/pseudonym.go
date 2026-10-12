package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
	// The shield index is encoded in private-use digits U+E010..U+E019, which
	// no name or ref regex can match.
	reShieldTok = regexp.MustCompile("\uE000([\uE010-\uE019]+)\uE001")
	// Jira keys with at most 6 digits only: longer runs look like booking refs.
	reShieldJira = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d{1,6}\b`)
	reShieldHash = regexp.MustCompile(`[A-Za-z0-9_.-]+#\d+`)
	// Date and time shields need non-digit boundaries (group 2 is the token).
	reShieldDate = regexp.MustCompile(`(^|\D)(\d{4}-\d{2}-\d{2})($|\D)`)
	reShieldTime = regexp.MustCompile(`(^|\D)(\d{2}:\d{2}(?::\d{2})?)($|\D)`)

	reMention = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)
	// group 1 is the preceding boundary, group 2 the @-name.
	reAtName = regexp.MustCompile(`(^|[^\p{L}\p{N}._%+-])(@\p{Lu}[\p{L}'’-]*(?:\s+\p{Lu}[\p{L}'’-]*){0,2})`)
	// group 1 is the label word, group 2 the token.
	reLabelledRef = regexp.MustCompile(`(?i)\b(booking|payment|order|pnr|bid|txn|transaction|ref|reference)\b(?:\s+(?:ref|reference|id|no|number)\b)?\.?\s*[:#=]?\s*(\d{2}:\d{2}(?::\d{2})?|[A-Za-z0-9][A-Za-z0-9_.-]*#\d+|[A-Za-z0-9][A-Za-z0-9-]{3,})`)
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

var reNameSuffix = regexp.MustCompile(`[\s]*(?:\([^()]*\)|\[[^\[\]]*\])[\s]*$`)

// stripNameSuffix removes trailing parenthetical or bracketed suffixes and
// trailing symbols/emoji ("Ann Lee (OOO)" -> "Ann Lee").
func stripNameSuffix(n string) string {
	for {
		t := strings.TrimSpace(n)
		t = reNameSuffix.ReplaceAllString(t, "")
		t = strings.TrimRightFunc(t, func(r rune) bool { return unicode.IsSymbol(r) || unicode.IsSpace(r) || r == '\uFE0F' || r == '\u200D' })
		if t == n {
			return t
		}
		n = t
	}
}

// AddPerson links a Slack user id and a display name (either may be empty) to
// one person alias, so <@id>, @Name and Name map to the same person-N. A name
// with a suffix such as "(Ops)" is registered both whole and without it.
func (p *Pseudonymizer) AddPerson(slackID, displayName string) {
	full := strings.TrimSpace(displayName)
	names := []string{}
	if short := stripNameSuffix(full); short != "" {
		names = append(names, short)
		if short != full {
			names = append(names, full)
		}
	}
	idKey, nameKey := "", ""
	if len(names) > 0 {
		nameKey = normAlias(names[0])
	}
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
	for _, n := range names {
		p.alias["person|"+normAlias(n)] = a
		p.persons = append(p.persons, personName{name: n, alias: a})
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
	for _, d := range strconv.Itoa(idx) {
		b.WriteRune('\uE010' + (d - '0'))
	}
	b.WriteString(shieldClose)
	return b.String()
}

func (p *Pseudonymizer) unshield(s string) string {
	return reShieldTok.ReplaceAllStringFunc(s, func(m string) string {
		t := m[len(shieldOpen) : len(m)-len(shieldClose)]
		idx := 0
		for _, r := range t {
			idx = idx*10 + int(r-'\uE010')
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
		re, err := regexp.Compile(`(?i)@?` + strings.Join(words, `\s+`))
		if err != nil {
			continue
		}
		p.nameRes = append(p.nameRes, nameRe{re: re, alias: pn.alias})
	}
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// replaceNames replaces whole-name matches of re. Boundaries are Unicode-aware
// and only required where the name's own edge is a word rune (RE2's \b is
// ASCII-only, so it cannot be used for Arabic or accented edges).
func replaceNames(re *regexp.Regexp, s string, fn func() string) string {
	var b strings.Builder
	pos, from := 0, 0
	for from <= len(s) {
		loc := re.FindStringIndex(s[from:])
		if loc == nil {
			break
		}
		start, end := from+loc[0], from+loc[1]
		first, _ := utf8.DecodeRuneInString(s[start:])
		if first == '@' {
			first, _ = utf8.DecodeRuneInString(s[start+1:])
		}
		last, _ := utf8.DecodeLastRuneInString(s[:end])
		ok := true
		if start > 0 && isWordRune(first) {
			prev, _ := utf8.DecodeLastRuneInString(s[:start])
			ok = !isWordRune(prev)
		}
		if ok && end < len(s) && isWordRune(last) {
			next, _ := utf8.DecodeRuneInString(s[end:])
			ok = !isWordRune(next)
		}
		if !ok {
			_, w := utf8.DecodeRuneInString(s[start:])
			from = start + w
			continue
		}
		b.WriteString(s[pos:start])
		b.WriteString(fn())
		pos, from = end, end
		if end == start {
			from++
		}
	}
	b.WriteString(s[pos:])
	return b.String()
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
		if r >= '\uE000' && r <= '\uE01F' {
			return '?'
		}
		return r
	}, s)

	// 0. labelled refs beat every shield (code-r1 owner decision): the token
	// after a label word is a ref even if it looks like a Jira key, repo#n,
	// date or time. A token without a digit is not a ref.
	s = p.labelledRefs(s)
	// 1. shield protected tokens.
	s = reShieldJira.ReplaceAllStringFunc(s, p.shield)
	s = reShieldHash.ReplaceAllStringFunc(s, p.shield)
	s = replaceGroup(reShieldDate, s, 2, p.shield)
	s = replaceGroup(reShieldTime, s, 2, p.shield)
	// 2. Slack mentions.
	s = reMention.ReplaceAllStringFunc(s, func(m string) string {
		id := reMention.FindStringSubmatch(m)[1]
		return p.shield(p.aliasFor("person", "id:"+strings.ToLower(id)))
	})
	// 3. registered display names.
	p.compileNames()
	for _, nr := range p.nameRes {
		alias := nr.alias
		s = replaceNames(nr.re, s, func() string { return p.shield(alias) })
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
	// 9. a value already aliased as a ref is aliased everywhere in the text.
	s = reRun.ReplaceAllStringFunc(s, func(tok string) string {
		if k := normAlias(tok); k != "" {
			if a, ok := p.alias["ref|"+k]; ok {
				return p.shield(a)
			}
		}
		return tok
	})
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
		if !hasDigitStr(s[t0:t1]) || (t1 < len(s) && s[t1] == '@') {
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
