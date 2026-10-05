package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ReindexHeuristicOptions keeps one fixed cutoff across dry runs, canaries and resumes.
type ReindexHeuristicOptions struct {
	Since      time.Time
	IntervalMS int
	MaxRows    int
	DryRun     bool
	Output     io.Writer
}

// ReindexHeuristicResult includes attempted progress even when the run stops.
// Done counts successful indexing calls; Skipped counts embedding failures.
type ReindexHeuristicResult struct {
	Done       int
	Skipped    int
	Eligible   int
	LastID     string
	SkippedIDs []string
	SampleIDs  []string
	Elapsed    time.Duration
}

// Validate rejects invalid flags before opening the database or calling a gateway.
func (opts ReindexHeuristicOptions) Validate() error {
	if opts.Since.IsZero() {
		return errors.New("--since is required (RFC3339)")
	}
	if opts.IntervalMS < 50 || opts.IntervalMS > 5000 {
		return errors.New("--interval-ms must be between 50 and 5000")
	}
	if opts.MaxRows < 0 {
		return errors.New("--max-rows must be non-negative")
	}
	return nil
}

const reindexHeuristicPopulation = `
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id=n.id
LEFT JOIN graph.artifact_bodies ab ON ab.node_id=n.id
WHERE n.deleted_at IS NULL
  AND n.type IN ('jira','gh_pr','cf_page')
  AND (ai.node_id IS NULL OR ai.refreshed_at < $1)
  AND (ai.summary_kind='heuristic'
       OR (ai.node_id IS NULL AND COALESCE(n.title,'') ~ '[^[:space:]]'
           AND COALESCE(ab.body_full,n.body,'')=''))
  AND n.id > $2`

// RunReindexHeuristic repairs resources directly, without queue continuations or
// the dispatcher's semaphore. Each shared index call owns its own transaction.
// The dedicated physical lock session is never replaced: losing it stops the
// run before the next node, although an already in-flight node can still finish.
func RunReindexHeuristic(ctx context.Context, deps Deps, opts ReindexHeuristicOptions) (result ReindexHeuristicResult, runErr error) {
	started := time.Now()
	out := opts.Output
	if out == nil {
		out = io.Discard
	}
	defer func() {
		result.Elapsed = time.Since(started)
		for _, id := range result.SkippedIDs {
			fmt.Fprintf(out, "skipped_id=%s\n", id)
		}
		fmt.Fprintf(out, "final done=%d skipped=%d eligible=%d elapsed=%s last_id=%s\n", result.Done, result.Skipped, result.Eligible, result.Elapsed.Round(time.Millisecond), result.LastID)
	}()
	if err := opts.Validate(); err != nil {
		return result, err
	}
	fmt.Fprintf(out, "cutoff=%s\n", opts.Since.UTC().Format(time.RFC3339Nano))
	if deps.DB == nil {
		return result, errors.New("reindex-heuristic: database is required")
	}
	if !opts.DryRun && deps.Gemini == nil {
		return result, errors.New("reindex-heuristic: gateway embedder is required")
	}
	conn, err := deps.DB.Acquire(ctx)
	if err != nil {
		return result, fmt.Errorf("acquire lock session: %w", err)
	}
	locked := false
	defer func() {
		if conn == nil {
			return
		}
		if !locked {
			conn.Release()
			return
		}
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var unlocked bool
		unlockErr := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock(hashtext('reindex_heuristic'))`).Scan(&unlocked)
		cancel()
		if unlockErr == nil && unlocked {
			conn.Release()
			return
		}
		// Release alone would retain a session lock on a pooled connection.
		// Hijack removes it from the pool before closing the physical session.
		physical := conn.Hijack()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		closeErr := physical.Close(closeCtx)
		closeCancel()
		if unlockErr == nil {
			unlockErr = errors.New("advisory lock was not held")
		}
		runErr = errors.Join(runErr, fmt.Errorf("unlock reindex-heuristic: %w", unlockErr), closeErr)
	}()
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('reindex_heuristic'))`).Scan(&locked); err != nil {
		// Even a failed lock query can have acquired its server-side lock before
		// losing the reply. Do not return that physical session to the pool.
		physical := conn.Hijack()
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = physical.Close(closeCtx)
		cancel()
		// The deferred release must not run on a hijacked pool connection.
		conn = nil
		return result, fmt.Errorf("acquire reindex-heuristic lock: %w", err)
	}
	if !locked {
		return result, errors.New("reindex-heuristic lock is held by another run")
	}
	var now time.Time
	if err := conn.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return result, fmt.Errorf("read database clock: %w", err)
	}
	if opts.Since.After(now) {
		return result, errors.New("--since is later than the database clock")
	}
	if !opts.DryRun {
		vector, err := deps.Gemini.EmbedWithOptions(ctx, "preflight", graphEmbeddingOptions())
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err != nil {
			return result, fmt.Errorf("embedding preflight: %w", err)
		}
		if len(vector) == 0 {
			return result, errors.New("embedding preflight returned an empty vector")
		}
	}
	cursor := ""
	consecutiveErrors := 0
	for opts.MaxRows == 0 || result.Done+result.Skipped < opts.MaxRows || opts.DryRun {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// Query on this exact acquired connection; never use pool Ping here.
		if _, err := conn.Exec(ctx, `SELECT 1`); err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, fmt.Errorf("lock session lost: %w", err)
		}
		rows, err := conn.Query(ctx, `SELECT n.id `+reindexHeuristicPopulation+` ORDER BY n.id LIMIT 1`, opts.Since, cursor)
		if err != nil {
			return result, fmt.Errorf("select reindex population: %w", err)
		}
		if !rows.Next() {
			err := rows.Err()
			rows.Close()
			if err != nil {
				return result, fmt.Errorf("read reindex population: %w", err)
			}
			return result, nil
		}
		var id string
		err = rows.Scan(&id)
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		if err != nil {
			return result, fmt.Errorf("scan reindex population: %w", err)
		}
		cursor = id
		result.Eligible++
		if opts.DryRun {
			if len(result.SampleIDs) < 20 {
				result.SampleIDs = append(result.SampleIDs, id)
				fmt.Fprintf(out, "sample_id=%s\n", id)
			}
			if opts.MaxRows > 0 && result.Eligible >= opts.MaxRows {
				return result, nil
			}
			continue
		}
		if result.Done+result.Skipped > 0 {
			timer := time.NewTimer(time.Duration(opts.IntervalMS) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return result, ctx.Err()
			case <-timer.C:
			}
			// The lock may have disappeared during sleep. Check immediately
			// before starting the next node as well as before population reads.
			if _, err := conn.Exec(ctx, `SELECT 1`); err != nil {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				return result, fmt.Errorf("lock session lost: %w", err)
			}
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.LastID = id
		err = indexArtifactNode(ctx, deps, id, true, true)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, err
		}
		if err != nil {
			var embedErr *artifactEmbedError
			if !errors.As(err, &embedErr) {
				return result, fmt.Errorf("index %s: %w", id, err)
			}
			deps.Logger.Warn().Err(err).Str("node_id", id).Msg("reindex-heuristic: embedding skipped")
			result.Skipped++
			result.SkippedIDs = append(result.SkippedIDs, id)
			consecutiveErrors++
		} else {
			result.Done++
			consecutiveErrors = 0
		}
		if (result.Done+result.Skipped)%50 == 0 {
			fmt.Fprintf(out, "progress done=%d skipped=%d elapsed=%s\n", result.Done, result.Skipped, time.Since(started).Round(time.Millisecond))
		}
		if consecutiveErrors > 20 {
			return result, errors.New("reindex-heuristic: more than 20 consecutive embedding errors")
		}
	}
	return result, nil
}
