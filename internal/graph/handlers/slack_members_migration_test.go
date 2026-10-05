package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog"
)

func TestSlackMembersMigration(t *testing.T) {
	db := openTestDB(t)
	membersReset(t, db)
	membersNode(t, db, "C1")
	sqlDB, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(migrationsDirFromHandlers), goose.WithAllowOutofOrder(true))
	if err != nil {
		t.Fatal(err)
	}
	const version int64 = 20261005042750
	ctx := context.Background()
	if _, err := provider.ApplyVersion(ctx, version, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		statuses, err := provider.Status(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		for _, s := range statuses {
			if s.Source.Version == version && s.State == goose.StatePending {
				if _, err := provider.ApplyVersion(ctx, version, true); err != nil {
					t.Error(err)
				}
			}
		}
	})
	membersExec(t, db, `INSERT INTO graph.member_scopes(eeid,scope) VALUES(1001,'slack:C1'),(1001,'slack:D1'),(1001,'jira:PAY')`)
	if _, err := provider.ApplyVersion(ctx, version, true); err != nil {
		t.Fatal(err)
	}
	var at time.Time
	if err := db.QueryRow(ctx, `SELECT refreshed_at FROM graph.member_scopes WHERE scope='slack:C1'`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if !at.Equal(time.Unix(0, 0)) {
		t.Fatalf("old grant freshness=%s", at)
	}
	check := func() {
		t.Helper()
		scopes, err := acl.NewBuilder(db, 0).For(ctx, 1001)
		if err != nil {
			t.Fatal(err)
		}
		if len(scopes) != 2 {
			t.Fatalf("scopes=%v", scopes)
		}
		for _, s := range scopes {
			if s == "slack:C1" {
				t.Fatal("unverified migrated grant visible")
			}
		}
	}
	check()
	client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	})
	if err := refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)(ctx, nil); !errors.Is(err, jobs.ErrFatal) {
		t.Fatalf("fatal=%v", err)
	}
	check()
	membersWant(t, db, "slack:C1")
	client = membersClient(t, func(w http.ResponseWriter, r *http.Request) { membersReply(w, []string{"U1"}, "") })
	if err := refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)(ctx, []byte(`{"force":true}`)); err != nil {
		t.Fatal(err)
	}
	scopes, err := acl.NewBuilder(db, 0).For(ctx, 1001)
	if err != nil || len(scopes) != 3 {
		t.Fatalf("refreshed scopes=%v err=%v", scopes, err)
	}
}
