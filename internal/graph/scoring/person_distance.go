package scoring

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LookupDistance returns the number of hops between two eeids in the
// reports_to tree, using the materialised graph.person_distance table.
// Returns math.MaxInt32 for unknown pairs.
func LookupDistance(ctx context.Context, db *pgxpool.Pool, a, b int) (int, error) {
	if a == b {
		return 0, nil
	}
	// Normalise order: smaller eeid is always a_eeid.
	if a > b {
		a, b = b, a
	}
	row := db.QueryRow(ctx, `
SELECT hops FROM graph.person_distance WHERE a_eeid=$1 AND b_eeid=$2
`, a, b)
	var hops int
	err := row.Scan(&hops)
	if errors.Is(err, pgx.ErrNoRows) {
		return math.MaxInt32, nil
	}
	if err != nil {
		return 0, err
	}
	return hops, nil
}
