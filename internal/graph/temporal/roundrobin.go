package temporal

import "time"

// Dated is a ranked candidate with an event time.
type Dated struct {
	ID    string
	At    time.Time
	Score float64
}

// RoundRobin spreads score-ordered candidates across the window so one busy
// week cannot crowd out the rest: the window is cut into n equal buckets,
// each candidate goes to its bucket (out-of-window ones to the nearest edge
// bucket), and the output takes the best remaining candidate from each
// non-empty bucket in turn until all are emitted. Relative order inside a
// bucket is preserved, so in must already be sorted best-first.
func RoundRobin(in []Dated, w Window, n int) []Dated {
	if n <= 1 || len(in) <= 1 || !w.End.After(w.Start) {
		return in
	}
	span := float64(w.End.Sub(w.Start))
	buckets := make([][]Dated, n)
	for _, d := range in {
		i := int(float64(d.At.Sub(w.Start)) / span * float64(n))
		if i < 0 {
			i = 0
		}
		if i >= n {
			i = n - 1
		}
		buckets[i] = append(buckets[i], d)
	}
	out := make([]Dated, 0, len(in))
	for len(out) < len(in) {
		for i := range buckets {
			if len(buckets[i]) == 0 {
				continue
			}
			out = append(out, buckets[i][0])
			buckets[i] = buckets[i][1:]
		}
	}
	return out
}
