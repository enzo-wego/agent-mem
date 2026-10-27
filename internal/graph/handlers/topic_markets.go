package handlers

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var marketCodePattern = regexp.MustCompile(`^[A-Z]{2}$`)

type marketTerms struct {
	code  string
	codes []string
	names []string
}

// ponytail: this is a heuristic for summaries written before markets existed.
// Provider-to-market mappings are assumptions, not authoritative geography.
var topicMarketTerms = []marketTerms{
	{code: "IN", codes: []string{"IN", "INR", "GST"}, names: []string{"india", "indian", "razorpay", "payu"}},
	{code: "SA", codes: []string{"SA", "KSA", "SAR", "ZATCA", "MADA"}, names: []string{"saudi", "tabby", "tamara"}},
	{code: "AE", codes: []string{"AE", "UAE", "AED"}, names: []string{"emirates"}},
	{code: "TH", codes: []string{"TH", "THB"}, names: []string{"thai", "thailand"}},
	{code: "PK", codes: []string{"PK", "PKR"}, names: []string{"pakistan"}},
	{code: "KW", codes: []string{"KW", "KWD", "KNET"}, names: []string{"kuwait"}},
	{code: "EG", codes: []string{"EG", "EGP"}, names: []string{"egypt"}},
}

func detectMarkets(text string) []string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	exact := make(map[string]struct{}, len(words))
	lower := make(map[string]struct{}, len(words))
	for _, word := range words {
		exact[word] = struct{}{}
		lower[strings.ToLower(word)] = struct{}{}
	}
	found := make(map[string]struct{})
	for _, market := range topicMarketTerms {
		matched := false
		for _, code := range market.codes {
			if _, ok := exact[code]; ok {
				matched = true
			}
		}
		for _, name := range market.names {
			if _, ok := lower[name]; ok {
				matched = true
			}
		}
		if matched {
			found[market.code] = struct{}{}
		}
	}
	return sortedMarketSet(found)
}

func parseMarkets(raw json.RawMessage) []string {
	out := []string{}
	if len(raw) == 0 || string(raw) == "null" {
		return out
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return out
	}
	found := make(map[string]struct{})
	for _, value := range values {
		var code string
		if json.Unmarshal(value, &code) == nil && marketCodePattern.MatchString(code) {
			found[code] = struct{}{}
		}
	}
	return sortedMarketSet(found)
}

func resolveMarkets(stored []string, hasStored bool, summary string) []string {
	if hasStored && stored != nil {
		return stored
	}
	return detectMarkets(summary)
}

func recentMarketCandidateMatches(sourceMarkets, candidateMarkets []string, sourceScope, candidateScope string) bool {
	if len(sourceMarkets) == 0 {
		return sourceScope == candidateScope
	}
	for _, source := range sourceMarkets {
		for _, candidate := range candidateMarkets {
			if source == candidate {
				return true
			}
		}
	}
	return false
}

func sortedMarketSet(found map[string]struct{}) []string {
	out := make([]string, 0, len(found))
	for code := range found {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}
