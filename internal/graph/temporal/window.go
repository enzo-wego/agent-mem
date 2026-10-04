// Package temporal parses time phrases out of a search query and spreads
// dated candidates across a window.
package temporal

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the runtime image may ship no zoneinfo; embed Go's copy
)

// Window is a half-open [Start, End) interval.
type Window struct {
	Start, End time.Time
	// Open marks a window with no real far edge ("this month", "since …",
	// since-only/until-only API calls): End is "now" or a placeholder, so
	// distance from the centre says nothing about relevance. Proximity is
	// neutral for it; Start/End still filter.
	Open bool
}

// Centre is the midpoint of the window.
func (w Window) Centre() time.Time {
	return w.Start.Add(w.End.Sub(w.Start) / 2)
}

// Proximity maps t to [0,1]: 1 at the window centre, 0 at either edge and
// beyond. 0.5 for a zero-length window (avoids a divide by zero) and for an
// Open window.
func (w Window) Proximity(t time.Time) float64 {
	if w.Open {
		return 0.5
	}
	half := float64(w.End.Sub(w.Start)) / 2
	if half <= 0 {
		return 0.5
	}
	d := float64(t.Sub(w.Centre()))
	if d < 0 {
		d = -d
	}
	p := 1 - d/half
	if p < 0 {
		return 0
	}
	return p
}

var months = map[string]time.Month{
	"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3,
	"apr": 4, "april": 4, "may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7,
	"aug": 8, "august": 8, "sep": 9, "sept": 9, "september": 9, "oct": 10,
	"october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
}

const monthAlt = `(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t|tember)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)`

// Leading prepositions are consumed with the phrase so "invoices in August"
// leaves "invoices", not "invoices in".
const lead = `(?:\b(?:in|during|on|from|for|of|around)\s+)?`

// Rules in priority order. Each regexp is case-insensitive and anchored on
// word boundaries; the whole match is removed from the query.
var (
	reISORange = regexp.MustCompile(`(?i)` + lead + `\b(\d{4}-\d{2}-\d{2})\s*(?:to|\.\.|–|-|until|through)\s*(\d{4}-\d{2}-\d{2})\b`)
	reISODate  = regexp.MustCompile(`(?i)` + lead + `\b(\d{4}-\d{2}-\d{2})\b`)
	reMonthYr  = regexp.MustCompile(`(?i)` + lead + `\b` + monthAlt + `\.?,?\s+(\d{4})\b`)
	reMonth    = regexp.MustCompile(`(?i)` + lead + `\b` + monthAlt + `\b\.?`)
	reLast     = regexp.MustCompile(`(?i)\b(?:in\s+the\s+|over\s+the\s+|during\s+the\s+)?(?:last|past|previous)\s+(week|month|quarter|year)\b`)
	reThis     = regexp.MustCompile(`(?i)\b(?:in\s+|during\s+)?this\s+(week|month|quarter|year)\b`)
	reQuarter  = regexp.MustCompile(`(?i)` + lead + `\bq([1-4])\s*(\d{4})\b`)
	reSince    = regexp.MustCompile(`(?i)\bsince\s+(?:(\d{4}-\d{2}-\d{2})|` + monthAlt + `(?:\s+(\d{4}))?)\b`)
	reRelDay   = regexp.MustCompile(`(?i)\b(yesterday|today)\b`)
)

// Parse finds the first time phrase in q and returns its window plus the query
// with the phrase removed, so the other arms embed the topic rather than the
// date. Rules are tried in a fixed priority order: "since …" first (its date
// or month must not be claimed by the narrower rules), then ISO range, ISO
// date, "<month> <year>", bare month, "last …", "this …", "Qn YYYY",
// yesterday/today. ok is false when nothing matched. Bare month names resolve
// to the most recent occurrence not after now.
func Parse(q string, now time.Time, loc *time.Location) (w Window, rest string, ok bool) {
	now = now.In(loc)
	type hit struct {
		loc []int
		w   Window
	}
	try := func(re *regexp.Regexp, build func(m []string) (Window, bool)) (hit, bool) {
		loc := re.FindStringSubmatchIndex(q)
		if loc == nil {
			return hit{}, false
		}
		m := make([]string, len(loc)/2)
		for i := range m {
			if loc[2*i] >= 0 {
				m[i] = q[loc[2*i]:loc[2*i+1]]
			}
		}
		w, good := build(m)
		if !good {
			return hit{}, false
		}
		return hit{loc: loc[:2], w: w}, true
	}

	rules := []func() (hit, bool){
		func() (hit, bool) {
			return try(reSince, func(m []string) (Window, bool) {
				var s time.Time
				switch {
				case m[1] != "":
					d, err := parseISO(m[1], loc)
					if err != nil {
						return Window{}, false
					}
					s = d
				default:
					mo := months[strings.ToLower(m[2])]
					y := now.Year()
					if m[3] != "" {
						y, _ = strconv.Atoi(m[3])
					} else if mo > now.Month() {
						y--
					}
					s = time.Date(y, mo, 1, 0, 0, 0, 0, loc)
				}
				return Window{Start: s, End: dayStart(now).AddDate(0, 0, 1), Open: true}, true
			})
		},
		func() (hit, bool) {
			return try(reISORange, func(m []string) (Window, bool) {
				a, e1 := parseISO(m[1], loc)
				b, e2 := parseISO(m[2], loc)
				if e1 != nil || e2 != nil || b.Before(a) {
					return Window{}, false
				}
				return Window{Start: a, End: b.AddDate(0, 0, 1)}, true
			})
		},
		func() (hit, bool) {
			return try(reISODate, func(m []string) (Window, bool) {
				a, err := parseISO(m[1], loc)
				if err != nil {
					return Window{}, false
				}
				return Window{Start: a, End: a.AddDate(0, 0, 1)}, true
			})
		},
		func() (hit, bool) {
			return try(reMonthYr, func(m []string) (Window, bool) {
				y, _ := strconv.Atoi(m[2])
				return monthWindow(y, months[strings.ToLower(m[1])], loc), true
			})
		},
		func() (hit, bool) {
			return try(reMonth, func(m []string) (Window, bool) {
				name := strings.ToLower(m[1])
				// "may" is a modal verb far more often than a month; only
				// take it with a preposition ("in May").
				if name == "may" && strings.EqualFold(strings.TrimSpace(m[0]), "may") {
					return Window{}, false
				}
				mo := months[name]
				y := now.Year()
				if mo > now.Month() {
					y--
				}
				return monthWindow(y, mo, loc), true
			})
		},
		func() (hit, bool) {
			return try(reLast, func(m []string) (Window, bool) {
				return lastWindow(strings.ToLower(m[1]), now), true
			})
		},
		func() (hit, bool) {
			return try(reThis, func(m []string) (Window, bool) {
				w := thisWindow(strings.ToLower(m[1]), now)
				w.Open = true
				return w, true
			})
		},
		func() (hit, bool) {
			return try(reQuarter, func(m []string) (Window, bool) {
				qn, _ := strconv.Atoi(m[1])
				y, _ := strconv.Atoi(m[2])
				s := time.Date(y, time.Month((qn-1)*3+1), 1, 0, 0, 0, 0, loc)
				return Window{Start: s, End: s.AddDate(0, 3, 0)}, true
			})
		},
		func() (hit, bool) {
			return try(reRelDay, func(m []string) (Window, bool) {
				d := dayStart(now)
				if strings.EqualFold(m[1], "yesterday") {
					d = d.AddDate(0, 0, -1)
				}
				return Window{Start: d, End: d.AddDate(0, 0, 1)}, true
			})
		},
	}
	for _, rule := range rules {
		h, matched := rule()
		if !matched {
			continue
		}
		rest = strings.TrimSpace(q[:h.loc[0]] + " " + q[h.loc[1]:])
		rest = strings.Join(strings.Fields(rest), " ")
		return h.w, rest, true
	}
	return Window{}, q, false
}

func parseISO(s string, loc *time.Location) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, loc)
}

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func monthWindow(y int, m time.Month, loc *time.Location) Window {
	s := time.Date(y, m, 1, 0, 0, 0, 0, loc)
	return Window{Start: s, End: s.AddDate(0, 1, 0)}
}

// weekStart is the Monday 00:00 in t's location on or before t.
func weekStart(t time.Time) time.Time {
	d := dayStart(t)
	off := (int(d.Weekday()) + 6) % 7
	return d.AddDate(0, 0, -off)
}

func quarterStart(t time.Time) time.Time {
	m := time.Month((int(t.Month())-1)/3*3 + 1)
	return time.Date(t.Year(), m, 1, 0, 0, 0, 0, t.Location())
}

// lastWindow is the previous complete calendar unit (last week = the Monday
// to Sunday before this week's Monday).
func lastWindow(unit string, now time.Time) Window {
	switch unit {
	case "week":
		e := weekStart(now)
		return Window{Start: e.AddDate(0, 0, -7), End: e}
	case "month":
		e := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return Window{Start: e.AddDate(0, -1, 0), End: e}
	case "quarter":
		e := quarterStart(now)
		return Window{Start: e.AddDate(0, -3, 0), End: e}
	default: // year
		e := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location())
		return Window{Start: e.AddDate(-1, 0, 0), End: e}
	}
}

// thisWindow is the current calendar unit up to (and including) today.
func thisWindow(unit string, now time.Time) Window {
	e := dayStart(now).AddDate(0, 0, 1)
	switch unit {
	case "week":
		return Window{Start: weekStart(now), End: e}
	case "month":
		return Window{Start: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()), End: e}
	case "quarter":
		return Window{Start: quarterStart(now), End: e}
	default: // year
		return Window{Start: time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location()), End: e}
	}
}
