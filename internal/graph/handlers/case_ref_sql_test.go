package handlers

import (
	"context"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCaseRefSQL(t *testing.T) {
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

	rows, err := pool.Query(ctx,
		`SELECT sid FROM unnest($1::text[]) sid WHERE `+caseRefSQL,
		[]string{"scheduler1", "pzxxyivdo4", "pzxxzkwud2", "pxx6xgkdtl", "a1234567890bcde", "PAY-1"})
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
	want := []string{"a1234567890bcde", "pxx6xgkdtl", "pzxxyivdo4", "pzxxzkwud2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("caseRefSQL kept %v, want %v", got, want)
	}
}
