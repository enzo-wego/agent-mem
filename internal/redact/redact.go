// Package redact masks personal data (emails, phone numbers, card-like
// numbers) in free text while leaving structural identifiers, payment refs and
// labelled booking refs readable. Pure functions, no state.
//
// Order matters: emails are masked first and everywhere (nothing is protected
// from email masking); then protected tokens are located; then phone and card
// candidates are masked outside the protected spans. Structure is protected by
// explicit token shapes, never by the character before a number.
package redact

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

// Payment-family refs. Single definition shared with the identifier extractor
// (internal/graph/handlers/identifiers.go).
var (
	// PaymentRef is <p|s|d><9 chars>.
	PaymentRef = regexp.MustCompile(`\b[psd][0-9b-oqrt-z]{9}\b`)
	// ActionRef is a<14 chars>.
	ActionRef = regexp.MustCompile(`\ba[0-9b-oqrt-z]{14}\b`)
)

const (
	emailMask = "[email]"
	phoneMask = "[phone]"
	cardMask  = "[card]"
)

var (
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reURL   = regexp.MustCompile(`https?://\S+`)
	reTS    = regexp.MustCompile(`\d{10}\.\d{6}`)
	reUUID  = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reFRef  = regexp.MustCompile(`F\.[a-z0-9]+`)
	// reBooking binds a label to exactly one value (group 1).
	reBooking = regexp.MustCompile(`(?i)\b(?:booking|order|pnr|itinerary)(?:\s*(?:ref(?:erence)?|no\.?|number|id|#))?\s*[:#=]\s*([A-Za-z0-9-]{5,20})`)
	reNodeID  = buildNodeIDRegexp()
	reWord    = regexp.MustCompile(`[A-Za-z]+`)
)

func buildNodeIDRegexp() *regexp.Regexp {
	names := make([]string, 0, len(ids.AllTypes))
	for _, t := range ids.AllTypes {
		names = append(names, regexp.QuoteMeta(string(t)))
	}
	// Longest first so slack_file wins over slack.
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	return regexp.MustCompile(`(?:` + strings.Join(names, "|") + "):[^\\s\"'`<>\\[\\]{}|,;]+")
}

var (
	phoneLabels = map[string]bool{"phone": true, "tel": true, "mobile": true, "whatsapp": true, "contact": true, "call": true, "hp": true}
	cardLabels  = map[string]bool{"card": true, "pan": true, "cc": true, "credit": true, "debit": true, "visa": true, "mastercard": true, "amex": true}
)

// Mask returns s with emails, phone numbers and card-like numbers replaced by
// [email], [phone] and [card].
func Mask(s string) string {
	if s == "" {
		return s
	}
	if strings.IndexByte(s, '@') >= 0 {
		s = reEmail.ReplaceAllString(s, emailMask)
	}
	if !hasDigit(s) {
		return s
	}
	prot := protectedSpans(s)
	var out strings.Builder
	out.Grow(len(s))
	flushed, pi, i := 0, 0, 0
	for i < len(s) {
		if pi < len(prot) && i >= prot[pi][0] {
			if i < prot[pi][1] {
				i = prot[pi][1]
			}
			pi++
			continue
		}
		limit := len(s)
		if pi < len(prot) {
			limit = prot[pi][0]
		}
		c := s[i]
		if isDigit(c) || (c == '+' && i+1 < limit && (isDigit(s[i+1]) || s[i+1] == '(')) {
			if end, mask := matchAt(s, i, limit); mask != "" {
				out.WriteString(s[flushed:i])
				out.WriteString(mask)
				i, flushed = end, end
				continue
			}
		}
		i++
	}
	if flushed == 0 {
		return s
	}
	out.WriteString(s[flushed:])
	return out.String()
}

// protectedSpans returns sorted, merged byte spans copied through unchanged by
// the phone/card step.
func protectedSpans(s string) [][2]int {
	var spans [][2]int
	add := func(a, b int) { spans = append(spans, [2]int{a, b}) }
	for _, m := range reURL.FindAllStringIndex(s, -1) {
		if leftOK(s, m[0]) {
			add(m[0], m[1])
		}
	}
	for _, m := range reNodeID.FindAllStringIndex(s, -1) {
		if leftOK(s, m[0]) {
			add(m[0], m[1])
		}
	}
	for _, m := range reTS.FindAllStringIndex(s, -1) {
		if leftOK(s, m[0]) && rightOK(s, m[1]) {
			add(m[0], m[1])
		}
	}
	for _, m := range reUUID.FindAllStringIndex(s, -1) {
		if leftOK(s, m[0]) && rightOK(s, m[1]) {
			add(m[0], m[1])
		}
	}
	for _, re := range []*regexp.Regexp{PaymentRef, ActionRef} {
		for _, m := range re.FindAllStringIndex(s, -1) {
			add(m[0], m[1])
		}
	}
	for _, m := range reFRef.FindAllStringIndex(s, -1) {
		if leftOK(s, m[0]) && rightOK(s, m[1]) {
			add(m[0], m[1])
		}
	}
	for _, m := range reBooking.FindAllStringSubmatchIndex(s, -1) {
		if v0, v1 := m[2], m[3]; v0 >= 0 && rightOK(s, v1) && (v1 >= len(s) || s[v1] != '-') {
			add(v0, v1)
		}
	}
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	merged := spans[:1]
	for _, sp := range spans[1:] {
		last := &merged[len(merged)-1]
		if sp[0] <= last[1] {
			if sp[1] > last[1] {
				last[1] = sp[1]
			}
			continue
		}
		merged = append(merged, sp)
	}
	return merged
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func hasDigit(s string) bool {
	for i := range len(s) {
		if isDigit(s[i]) {
			return true
		}
	}
	return false
}

func isAlnumRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// leftOK: the byte before a is not a letter, digit or underscore.
func leftOK(s string, a int) bool {
	if a == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:a])
	return !isAlnumRune(r) && r != '_'
}

// rightOK: the byte at b is not a letter, digit or underscore.
func rightOK(s string, b int) bool {
	if b >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[b:])
	return !isAlnumRune(r) && r != '_'
}

// A candidate is a digit group not directly adjacent to a letter or digit.
func candLeftOK(s string, a int) bool {
	if a == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:a])
	return !isAlnumRune(r)
}

func candRightOK(s string, b int) bool {
	if b >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[b:])
	return !isAlnumRune(r)
}

// Separators allowed between digit groups of a phone number: one separator
// character, optionally with one parenthesis on either side of it.
var phoneSeps = map[string]bool{
	"": true, " ": true, "-": true, ".": true,
	"(": true, ")": true, " (": true, ") ": true, ")-": true, ").": true, "-(": true, ".(": true,
}

const maxDigits = 19

type group struct {
	end    int  // byte offset just past the digit run
	digits int  // cumulative digits through this group
	cardOK bool // separators so far are single spaces or hyphens only
}

// matchAt tries to match a phone or card candidate starting at i (a digit, or
// '+'). Longest span wins; phone beats card on the same span.
func matchAt(s string, i, limit int) (end int, mask string) {
	if !candLeftOK(s, i) {
		return 0, ""
	}
	p := i
	plus := false
	opens := 0
	if s[p] == '+' {
		plus = true
		p++
		if p < limit && s[p] == '(' {
			opens++
			p++
		}
	}
	var groups []group
	cardOK := true // separators so far are single space/hyphen only
	for p < limit && isDigit(s[p]) {
		q := p
		for q < limit && isDigit(s[q]) {
			q++
		}
		prev := 0
		if n := len(groups); n > 0 {
			prev = groups[n-1].digits
		}
		digits := prev + (q - p)
		if digits > maxDigits {
			break
		}
		g := group{end: q, digits: digits, cardOK: cardOK}
		groups = append(groups, g)
		// Look for a separator then another digit group.
		r := q
		for r < limit && r-q < 2 && !isDigit(s[r]) && strings.IndexByte(" -.()", s[r]) >= 0 {
			r++
		}
		if r >= limit || !isDigit(s[r]) || !phoneSeps[s[q:r]] {
			break
		}
		sep := s[q:r]
		opens += strings.Count(sep, "(")
		if opens > 1 {
			break
		}
		if sep != " " && sep != "-" {
			cardOK = false
		}
		p = r
	}
	if len(groups) == 0 {
		return 0, ""
	}
	for k := len(groups) - 1; k >= 0; k-- {
		g := groups[k]
		e := g.end
		// A trailing ')' closes the one opened parenthesis.
		if e < len(s) && s[e] == ')' && strings.Count(s[i:e], "(") == 1 && strings.Count(s[i:e], ")") == 0 && e+1 <= limit {
			e++
		}
		if !candRightOK(s, e) {
			continue
		}
		n := g.digits
		if m := classify(s, i, e, plus, n, g.cardOK && !strings.ContainsAny(s[i:e], "().")); m != "" {
			return e, m
		}
	}
	return 0, ""
}

func classify(s string, start, end int, plus bool, n int, cardShape bool) string {
	if plus && n >= 8 && n <= 15 {
		return phoneMask
	}
	label := nearestLabel(s, start)
	if label != "" && phoneLabels[label] && n >= 7 && n <= 15 {
		return phoneMask
	}
	if !cardShape {
		return ""
	}
	digits := digitsOf(s[start:end])
	if n >= 13 && n <= 19 && luhn(digits) {
		return cardMask
	}
	if label != "" && cardLabels[label] && n >= 12 && n <= 19 {
		return cardMask
	}
	return ""
}

func digitsOf(s string) string {
	b := make([]byte, 0, len(s))
	for i := range len(s) {
		if isDigit(s[i]) {
			b = append(b, s[i])
		}
	}
	return string(b)
}

// nearestLabel returns the lower-cased label word (phone or card family)
// nearest before start, within 20 bytes and with no digit between it and the
// candidate. Empty when none.
func nearestLabel(s string, start int) string {
	from := start - 20
	if from < 0 {
		from = 0
	}
	for from > 0 && from < start && isASCIILetter(s[from-1]) && isASCIILetter(s[from]) {
		from-- // do not cut a word in half
	}
	win := s[from:start]
	if d := strings.LastIndexAny(win, "0123456789"); d >= 0 {
		win = win[d+1:]
	}
	label := ""
	for _, w := range reWord.FindAllString(win, -1) {
		lw := strings.ToLower(w)
		if phoneLabels[lw] || cardLabels[lw] {
			label = lw
		}
	}
	return label
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func luhn(digits string) bool {
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return len(digits) > 0 && sum%10 == 0
}
