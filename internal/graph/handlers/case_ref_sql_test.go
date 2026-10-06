package handlers

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func queryCaseRef(t *testing.T, sql string, in []string) []string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var db string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		t.Fatal(err)
	}
	if db != "agentmem_test" {
		t.Fatalf("refusing to run against database %q, want agentmem_test", db)
	}
	rows, err := pool.Query(ctx, `SELECT sid FROM unnest($1::text[]) sid WHERE `+sql, in)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	return got
}

func TestNotPaymentRefStemsShape(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]+$`)
	for k := range notPaymentRefStems {
		if !re.MatchString(k) {
			t.Errorf("stem %q must match ^[a-z]+$", k)
		}
	}
}

func TestCaseRefSQLEmptyStems(t *testing.T) {
	got := queryCaseRef(t, buildCaseRefSQL(nil), []string{"scheduler1", "pzxxyivdo4"})
	want := []string{"pzxxyivdo4", "scheduler1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("empty stem list kept %v, want %v", got, want)
	}
}

func TestCaseRefSQL(t *testing.T) {
	got := queryCaseRef(t, caseRefSQL,
		[]string{"scheduler1", "pzxxyivdo4", "pzxxzkwud2", "pxx6xgkdtl", "a1234567890bcde", "PAY-1"})
	want := []string{"a1234567890bcde", "pxx6xgkdtl", "pzxxyivdo4", "pzxxzkwud2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("caseRefSQL kept %v, want %v", got, want)
	}
}
