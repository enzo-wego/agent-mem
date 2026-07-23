package handlers

import "testing"

// The production seed: ignore staging/noise channels, keep only tax/payment
// problems in task-alerts-production (drop the green-check success heartbeats).
const seedChannelFilters = `{
  "ignore": ["C01T60D80JV", "C0A7D29E5ED", "C0B1BR522F5", "C0AJ3JPRA9L", "C02AD7A21UH", "C029TRHS5HU"],
  "incident_only": {"C08S954G2LX": ["PagerDuty"]},
  "keep_regex": {"CPP5EH3A8": "(?i)pending.?payment|process[- ]?taxes"},
  "drop_regex": {"CPP5EH3A8": "(?i)white_check_mark[\\s\\S]*->\\s*200"}
}`

func TestChannelFiltersContentSkip(t *testing.T) {
	f := compileChannelFilters(seedChannelFilters)

	cases := []struct {
		name     string
		channel  string
		body     string
		wantSkip bool
		outcome  string
	}{
		{"ignored staging channel", "C01T60D80JV", "anything at all", true, "skipped_ignored_channel"},
		{"ignored itops channel", "C0A7D29E5ED", "some AI news", true, "skipped_ignored_channel"},
		{"unfiltered channel passes", "C05RNSE8TBR", "payments-team chatter", false, ""},
		{
			"task-alerts: tax success heartbeat dropped",
			"CPP5EH3A8",
			":white_check_mark: process-taxes triggered 2026-07-23T04:17:02Z [from=2026-07-22 to=2026-07-23] -> 200",
			true, "skipped_off_topic",
		},
		{
			"task-alerts: tax warning kept",
			"CPP5EH3A8",
			":warning: process-taxes hourly cron: VPN tunnel could not be (re)established — run skipped",
			false, "",
		},
		{
			"task-alerts: pending-payment failure kept",
			"CPP5EH3A8",
			":red_circle: Task Failed (v2) (https://scheduler/dags/payments.process-pending-payments/…)",
			false, "",
		},
		{
			"task-alerts: unrelated alert dropped (off-topic)",
			"CPP5EH3A8",
			":red_circle: Task Failed (v2) macherly.shopcash-conversion.import-jumia",
			true, "skipped_off_topic",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip, outcome := f.contentSkip(tc.channel, tc.body)
			if skip != tc.wantSkip || outcome != tc.outcome {
				t.Fatalf("contentSkip(%q) = (%v, %q); want (%v, %q)",
					tc.channel, skip, outcome, tc.wantSkip, tc.outcome)
			}
		})
	}
}

func TestChannelFiltersIncidentOnly(t *testing.T) {
	f := compileChannelFilters(seedChannelFilters)
	if authors, ok := f.incidentOnly["C08S954G2LX"]; !ok || len(authors) != 1 || authors[0] != "PagerDuty" {
		t.Fatalf("incident_only[payments-alerts] = %v, %v; want [PagerDuty]", authors, ok)
	}
	if _, ok := f.incidentOnly["CPP5EH3A8"]; ok {
		t.Fatalf("task-alerts should not be incident_only")
	}
}

func TestChannelFiltersBadConfigIsNoOp(t *testing.T) {
	for _, raw := range []string{"", "not json", `{"keep_regex":{"C1":"("}}`} {
		f := compileChannelFilters(raw)
		if skip, _ := f.contentSkip("C1", "anything"); skip {
			t.Fatalf("bad config %q should not skip anything", raw)
		}
	}
}
