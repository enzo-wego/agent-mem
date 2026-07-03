// Package ids constructs and parses deterministic, human-readable graph node
// identifiers of the shape "<type>:<natural_key>".
package ids

import (
	"fmt"
	"regexp"
	"strings"
)

// NodeType is the type prefix embedded in every node ID.
type NodeType string

const (
	TypeSlackThread NodeType = "slack"
	TypeJira        NodeType = "jira"
	TypeGHPR        NodeType = "gh_pr"
	TypeCFPage      NodeType = "cf"
	TypePagerDuty   NodeType = "pagerduty"
	TypeDatadog     NodeType = "datadog"
	TypeSentry      NodeType = "sentry"
	TypeGWSDoc         NodeType = "gws_doc"
	TypeWegoHub        NodeType = "wegohub"
	TypeClaudeArtifact NodeType = "claude_artifact"
	TypeJiraAttachment NodeType = "jira_attachment"
	TypeSlackFile   NodeType = "slack_file"
	TypePartner     NodeType = "partner"
	TypeFeature     NodeType = "feature"
	TypeStatus      NodeType = "status"
	TypeCurrency    NodeType = "currency"
	TypeCodeFile    NodeType = "code_file"
	TypePerson      NodeType = "person"
	TypeUserGroup   NodeType = "usergroup"
)

var (
	reJiraKey       = regexp.MustCompile(`^[A-Z][A-Z0-9]+-\d+$`)
	rePagerDutyID   = regexp.MustCompile(`^[A-Z0-9]+$`)
	reSentryID      = regexp.MustCompile(`^[A-Z0-9_\-]+$`)
	reGHRepo        = regexp.MustCompile(`^[a-zA-Z0-9_.\-]+/[a-zA-Z0-9_.\-]+$`)
	reWegoHubSlug   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	reClaudeArtID   = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}$`)
	datadogTypes    = map[string]bool{"monitor": true, "dashboard": true, "log": true}
)

// SlackThread builds "slack:<channel>:<ts>". ts is the parent thread_ts.
func SlackThread(channel, ts string) string {
	return fmt.Sprintf("slack:%s:%s", channel, ts)
}

// SlackMessage builds "slack:<channel>:<ts>" (same shape; semantic difference only).
func SlackMessage(channel, ts string) string {
	return fmt.Sprintf("slack:%s:%s", channel, ts)
}

// SlackFile builds "slack_file:<file_id>".
func SlackFile(fileID string) string {
	return fmt.Sprintf("slack_file:%s", fileID)
}

// Jira builds "jira:<key>". Validates uppercase project key + dash + digits.
func Jira(key string) (string, error) {
	if !reJiraKey.MatchString(key) {
		return "", fmt.Errorf("ids: invalid Jira key %q (want PROJECT-123)", key)
	}
	return fmt.Sprintf("jira:%s", key), nil
}

// GHPR builds "gh_pr:<repo>#<number>". Validates repo shape.
func GHPR(repo string, number int) (string, error) {
	if !reGHRepo.MatchString(repo) {
		return "", fmt.Errorf("ids: invalid GitHub repo %q (want org/repo)", repo)
	}
	if number <= 0 {
		return "", fmt.Errorf("ids: PR number must be positive, got %d", number)
	}
	return fmt.Sprintf("gh_pr:%s#%d", repo, number), nil
}

// CFPage builds "cf:<id>".
func CFPage(id int64) string {
	return fmt.Sprintf("cf:%d", id)
}

// PagerDuty builds "pagerduty:<id>". ID is uppercase alphanumeric.
func PagerDuty(id string) (string, error) {
	if !rePagerDutyID.MatchString(id) || id == "" {
		return "", fmt.Errorf("ids: invalid PagerDuty ID %q (want uppercase alphanumeric)", id)
	}
	return fmt.Sprintf("pagerduty:%s", id), nil
}

// Datadog builds "datadog:<objectType>:<id>".
// Object types: "monitor", "dashboard", "log".
func Datadog(objectType string, id int64) (string, error) {
	if !datadogTypes[objectType] {
		return "", fmt.Errorf("ids: unknown Datadog object type %q (want monitor|dashboard|log)", objectType)
	}
	return fmt.Sprintf("datadog:%s:%d", objectType, id), nil
}

// Sentry builds "sentry:<issueID>".
func Sentry(issueID string) (string, error) {
	if issueID == "" {
		return "", fmt.Errorf("ids: Sentry issue ID must not be empty")
	}
	if !reSentryID.MatchString(issueID) {
		return "", fmt.Errorf("ids: invalid Sentry issue ID %q", issueID)
	}
	return fmt.Sprintf("sentry:%s", issueID), nil
}

// GWSDoc builds "gws_doc:<driveID>".
func GWSDoc(driveID string) string {
	return fmt.Sprintf("gws_doc:%s", driveID)
}

// WegoHub builds "wegohub:<slug>". Validates the Wego Hub slug rules:
// lowercase letters, digits and hyphens, starting and ending alphanumeric,
// max 64 chars.
func WegoHub(slug string) (string, error) {
	if !reWegoHubSlug.MatchString(slug) {
		return "", fmt.Errorf("ids: invalid Wego Hub slug %q (lowercase alnum + hyphens, max 64)", slug)
	}
	return fmt.Sprintf("wegohub:%s", slug), nil
}

// ClaudeArtifact builds "claude_artifact:<id>". id is the artifact UUID/slug
// from a claude.ai artifact URL.
func ClaudeArtifact(id string) (string, error) {
	if !reClaudeArtID.MatchString(id) {
		return "", fmt.Errorf("ids: invalid Claude artifact id %q", id)
	}
	return fmt.Sprintf("claude_artifact:%s", id), nil
}

// Partner builds "partner:<slug>". Name is lowercased; spaces become hyphens.
func Partner(name string) string {
	slug := strings.ToLower(name)
	slug = strings.ReplaceAll(slug, " ", "-")
	return fmt.Sprintf("partner:%s", slug)
}

// Feature builds "feature:<slug>". Name is lowercased; spaces become underscores.
func Feature(name string) string {
	slug := strings.ToLower(name)
	slug = strings.ReplaceAll(slug, " ", "_")
	return fmt.Sprintf("feature:%s", slug)
}

// Status builds "status:<slug>". Name is lowercased.
func Status(name string) string {
	return fmt.Sprintf("status:%s", strings.ToLower(name))
}

// Currency builds "currency:<code>". Code is lowercased.
func Currency(code string) string {
	return fmt.Sprintf("currency:%s", strings.ToLower(code))
}

// CodeFile builds "code_file:<repoPath>".
func CodeFile(repoPath string) string {
	return fmt.Sprintf("code_file:%s", repoPath)
}

// Person builds "person:<email>". Email is lowercased.
func Person(email string) string {
	return fmt.Sprintf("person:%s", strings.ToLower(email))
}

// UserGroup builds "usergroup:<id>".
func UserGroup(id string) string {
	return fmt.Sprintf("usergroup:%s", id)
}

// ParseType returns the NodeType prefix of any node ID.
// Returns ("", false) if malformed.
func ParseType(nodeID string) (NodeType, bool) {
	idx := strings.Index(nodeID, ":")
	if idx <= 0 {
		return "", false
	}
	prefix := nodeID[:idx]
	switch NodeType(prefix) {
	case TypeSlackThread, TypeJira, TypeGHPR, TypeCFPage, TypePagerDuty,
		TypeDatadog, TypeSentry, TypeGWSDoc, TypeWegoHub, TypeClaudeArtifact, TypeJiraAttachment,
		TypeSlackFile, TypePartner,
		TypeFeature, TypeStatus, TypeCurrency, TypeCodeFile, TypePerson,
		TypeUserGroup:
		return NodeType(prefix), true
	}
	return "", false
}

// ParseNaturalKey returns everything after the first ":" segment that defines
// the type. For "datadog:monitor:133274814" -> "monitor:133274814".
// For "jira:PAY-2128" -> "PAY-2128".
func ParseNaturalKey(nodeID string) (string, bool) {
	idx := strings.Index(nodeID, ":")
	if idx <= 0 || idx == len(nodeID)-1 {
		return "", false
	}
	return nodeID[idx+1:], true
}
