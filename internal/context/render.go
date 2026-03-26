package context

import (
	"fmt"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/database"
)

// render formats observations and summaries as markdown for context injection.
func render(observations []database.ObservationRow, summaries []database.SummaryRow, fullCount int) string {
	var b strings.Builder
	b.WriteString("<agent-mem-context>\n")

	if len(observations) > 0 {
		renderObservations(&b, observations, fullCount)
	}

	if len(summaries) > 0 {
		renderSummaries(&b, summaries)
	}

	b.WriteString("</agent-mem-context>")
	return b.String()
}

func renderObservations(b *strings.Builder, observations []database.ObservationRow, fullCount int) {
	b.WriteString("# Recent Activity\n\n")

	// Group by date
	type dateGroup struct {
		date         string
		observations []database.ObservationRow
	}

	var groups []dateGroup
	var currentDate string
	for _, obs := range observations {
		d := obs.CreatedAt.Format("Jan 2, 2006")
		if d != currentDate {
			groups = append(groups, dateGroup{date: d})
			currentDate = d
		}
		groups[len(groups)-1].observations = append(groups[len(groups)-1].observations, obs)
	}

	globalIdx := 0
	for _, g := range groups {
		fmt.Fprintf(b, "### %s\n\n", g.date)

		// Table header
		b.WriteString("| # | Time | Type | Title |\n")
		b.WriteString("|---|------|------|-------|\n")

		startIdx := globalIdx
		for _, obs := range g.observations {
			globalIdx++
			timeStr := obs.CreatedAt.Format("3:04 PM")
			fmt.Fprintf(b, "| %d | %s | %s | %s |\n", globalIdx, timeStr, obs.Type, obs.Title)
		}
		b.WriteString("\n")

		// Full narratives for top N overall
		idx := startIdx
		for _, obs := range g.observations {
			idx++
			if idx > fullCount {
				break
			}
			fmt.Fprintf(b, "#### [%d] %s\n", idx, obs.Title)
			if obs.Narrative != "" {
				fmt.Fprintf(b, "> %s\n", obs.Narrative)
			}
			if len(obs.FilesModified) > 0 {
				fmt.Fprintf(b, "> Files: %s\n", strings.Join(obs.FilesModified, ", "))
			} else if len(obs.FilesRead) > 0 {
				fmt.Fprintf(b, "> Files: %s\n", strings.Join(obs.FilesRead, ", "))
			}
			b.WriteString("\n")
		}
	}
}

func renderSummaries(b *strings.Builder, summaries []database.SummaryRow) {
	b.WriteString("### Recent Sessions\n\n")
	for _, s := range summaries {
		timeStr := s.CreatedAt.Format(time.DateTime)
		parts := []string{}
		if s.Request != "" {
			parts = append(parts, s.Request)
		}
		if s.Completed != "" {
			parts = append(parts, "Completed: "+s.Completed)
		}
		if s.Learned != "" {
			parts = append(parts, "Learned: "+s.Learned)
		}
		detail := strings.Join(parts, ". ")
		fmt.Fprintf(b, "- **Session %s**: %s\n", timeStr, detail)
	}
	b.WriteString("\n")
}
