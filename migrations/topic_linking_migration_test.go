package migrations

import (
	"os"
	"strings"
	"testing"
)

func TestTopicLinkingMigration(t *testing.T) {
	body, err := os.ReadFile("20260709000001_topic_linking.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(body)
	for _, want := range []string{
		"ALTER TABLE graph.edges ADD COLUMN IF NOT EXISTS metadata JSONB",
		"ALTER COLUMN embedding TYPE halfvec(3072)",
		"USING hnsw (embedding halfvec_cosine_ops)",
		"TRUNCATE graph.artifact_index, graph.artifact_bodies, graph.thread_summaries",
		"CREATE TABLE IF NOT EXISTS graph.alert_fingerprints",
		"CREATE TABLE IF NOT EXISTS graph.alert_fingerprint_events",
		"CREATE TABLE IF NOT EXISTS graph.topic_link_judgments",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("migration missing %q", want)
		}
	}
}
