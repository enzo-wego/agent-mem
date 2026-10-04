package handlers_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const stubSlackThread = "slack:CSTUB1:1700000000.000100"

func seedStubSlackThread(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	spNode(t, pool, "jira:STUB-1", "jira", "Stub ticket", "", "public", "", "{}", 0, false)
	spNode(t, pool, stubSlackThread, "slack", "", "", "", "", "{}", 0, false)
	seedEdge(t, pool, "jira:STUB-1", stubSlackThread, "REFERENCES")
	spChannel(t, pool, "CSTUB1", "stub-channel")
}

func TestNeighborsCards_StubSlackThreadNamesChannel(t *testing.T) {
	pool := testDB(t)
	seedStubSlackThread(t, pool)
	rows, _ := spNeighbors(t, pool, "jira:STUB-1", "cards=1")
	node := jpRow(t, rows, stubSlackThread)
	if node["channel"] != "stub-channel" {
		t.Errorf("channel = %v, want stub-channel", node["channel"])
	}
	if node["title"] != "Slack thread in #stub-channel" {
		t.Errorf("title = %v, want Slack thread in #stub-channel", node["title"])
	}
}

func TestNeighborsCards_StubSlackThreadHidesPrivateName(t *testing.T) {
	pool := testDB(t)
	seedStubSlackThread(t, pool)
	jpAsker(t, pool)
	rows, _ := jpNeighbors(t, pool, "jira:STUB-1", "cards=1", "USPASK1")
	node := jpRow(t, rows, stubSlackThread)
	if channel, present := node["channel"]; present && channel != "" {
		t.Errorf("private channel leaked: %v", channel)
	}
	if node["title"] != "Slack thread" {
		t.Errorf("title = %v, want Slack thread", node["title"])
	}
}
