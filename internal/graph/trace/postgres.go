package trace

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct{ DB *pgxpool.Pool }

func (s PostgresStore) Write(ctx context.Context, r Record) error {
	asker, err := json.Marshal(r.AskerInput)
	if err != nil {
		return err
	}
	request, err := json.Marshal(r.Request)
	if err != nil {
		return err
	}
	items, err := json.Marshal(r.Items)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO graph.context_traces
(id,created_at,route,build_rev,caller,status,error,asker_input,acl_mode,request,items,items_truncated,response_sha256,latency_ms)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, r.ID, r.CreatedAt, r.Route, r.BuildRev, r.Caller, r.Status, r.Error, asker, r.ACLMode, request, items, r.ItemsTruncated, r.ResponseSHA256, r.LatencyMS)
	return err
}
func (s PostgresStore) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.DB.Exec(ctx, `DELETE FROM graph.context_traces WHERE created_at < $1`, cutoff)
	return tag.RowsAffected(), err
}
