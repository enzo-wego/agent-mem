package handlers

import "regexp"

// URL patterns shared by ingest_url.go and any other handler that needs to
// derive a canonical node ID directly from a raw URL.

var (
	slackURLPattern          = regexp.MustCompile(`\bwego\.slack\.com/archives/(C\w+)/p(\d+)\b`)
	ghPRURLPattern           = regexp.MustCompile(`\bgithub\.com/(wego/[\w-]+)/pull/(\d+)\b`)
	cfPageURLPattern         = regexp.MustCompile(`\bwegomushi\.atlassian\.net/wiki/[^\s]*?pages/(\d+)\b`)
	pdIncidentURLPattern     = regexp.MustCompile(`\bwegotravel\.pagerduty\.com/incidents/(\w+)\b`)
	ddMonitorURLPattern      = regexp.MustCompile(`\bapp\.datadoghq\.com/monitors/(\d+)\b`)
	sentryIssueURLPattern    = regexp.MustCompile(`\bsentry\.io/[\w-]+/[\w-]+/issues/(\w+)/?`)
	gwsDocURLPattern         = regexp.MustCompile(`\bdocs\.google\.com/document/d/([\w-]+)\b`)
	wegoHubURLPattern        = regexp.MustCompile(`^https://internal\.wego\.com/hub/apps/([a-z0-9][a-z0-9-]*)\b`)
	claudeArtifactURLPattern = regexp.MustCompile(`^https://claude\.ai/(?:public/artifacts|code/artifact)/([A-Za-z0-9_-]{8,})\b`)
)
