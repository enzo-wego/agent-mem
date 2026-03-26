package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/gemini"

	_ "modernc.org/sqlite"
)

func runBackfillEmbeddings(databaseURL, geminiAPIKey, embeddingModel string, embeddingDims int) error {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	client := gemini.NewClient(geminiAPIKey, "", embeddingModel, embeddingDims)
	return backfillEmbeddings(ctx, pool, client)
}

func runMigrate(sqlitePath, databaseURL, geminiAPIKey string) error {
	ctx := context.Background()

	// Open SQLite
	sqliteDB, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer sqliteDB.Close()

	// Verify SQLite is readable
	var sqliteVer string
	if err := sqliteDB.QueryRow("SELECT sqlite_version()").Scan(&sqliteVer); err != nil {
		return fmt.Errorf("sqlite not readable: %w", err)
	}
	log.Info().Str("sqlite_version", sqliteVer).Str("path", sqlitePath).Msg("SQLite opened")

	// Connect to PostgreSQL
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	log.Info().Msg("PostgreSQL connected (schema managed by goose migrations)")

	// Migrate tables in FK-safe order
	if err := migrateSessions(ctx, sqliteDB, pool); err != nil {
		return fmt.Errorf("migrate sessions: %w", err)
	}
	if err := migrateObservations(ctx, sqliteDB, pool); err != nil {
		return fmt.Errorf("migrate observations: %w", err)
	}
	if err := migrateSummaries(ctx, sqliteDB, pool); err != nil {
		return fmt.Errorf("migrate summaries: %w", err)
	}
	if err := migratePrompts(ctx, sqliteDB, pool); err != nil {
		return fmt.Errorf("migrate prompts: %w", err)
	}

	// Backfill embeddings if Gemini key provided
	if geminiAPIKey != "" {
		client := gemini.NewClient(geminiAPIKey, "", "gemini-embedding-001", 768)
		if err := backfillEmbeddings(ctx, pool, client); err != nil {
			log.Warn().Err(err).Msg("Embedding backfill had errors")
		}
	} else {
		log.Warn().Msg("No Gemini API key, skipping embedding backfill")
	}

	// Verify
	return verify(ctx, sqliteDB, pool)
}

// msToSec converts a millisecond epoch to seconds.
// claude-mem SQLite stores epochs in milliseconds, agent-mem PG uses seconds.
func msToSec(ms int64) int64 {
	if ms > 1e12 { // clearly milliseconds (year > 33658 in seconds)
		return ms / 1000
	}
	return ms // already seconds
}

// msToSecPtr converts a nullable millisecond epoch to seconds.
func msToSecPtr(ms *int64) *int64 {
	if ms == nil {
		return nil
	}
	v := msToSec(*ms)
	return &v
}

func migrateSessions(ctx context.Context, sqlite *sql.DB, pg *pgxpool.Pool) error {
	// claude-mem SQLite schema: no sync_version/sync_source columns
	rows, err := sqlite.Query(`
		SELECT content_session_id, memory_session_id, project, user_prompt,
		       started_at, started_at_epoch, completed_at, completed_at_epoch, status,
		       sync_id
		FROM sdk_sessions
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var contentID, project, status string
		var memoryID, userPrompt, syncID *string
		var startedAt, completedAt *string
		var startedEpoch int64
		var completedEpoch *int64

		if err := rows.Scan(&contentID, &memoryID, &project, &userPrompt,
			&startedAt, &startedEpoch, &completedAt, &completedEpoch, &status,
			&syncID); err != nil {
			log.Warn().Err(err).Msg("Skip session row")
			continue
		}

		// Convert ms epochs to seconds
		startedEpoch = msToSec(startedEpoch)
		completedEpoch = msToSecPtr(completedEpoch)

		var startedTime, completedTime *time.Time
		if startedAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *startedAt); err == nil {
				startedTime = &t
			}
		}
		if startedTime == nil {
			t := time.Unix(startedEpoch, 0)
			startedTime = &t
		}
		if completedAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *completedAt); err == nil {
				completedTime = &t
			}
		}

		_, err := pg.Exec(ctx, `
			INSERT INTO sdk_sessions (content_session_id, memory_session_id, project, user_prompt,
				started_at, started_at_epoch, completed_at, completed_at_epoch, status,
				sync_id, sync_version, sync_source)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0, NULL)
			ON CONFLICT (content_session_id) DO NOTHING
		`, contentID, memoryID, project, userPrompt,
			startedTime, startedEpoch, completedTime, completedEpoch, status,
			syncID)
		if err != nil {
			log.Warn().Err(err).Str("session", contentID).Msg("Failed to insert session")
			continue
		}
		count++
		if count%1000 == 0 {
			log.Info().Int("count", count).Msg("Sessions migrated")
		}
	}
	log.Info().Int("total", count).Msg("Sessions migration complete")
	return nil
}

func migrateObservations(ctx context.Context, sqlite *sql.DB, pg *pgxpool.Pool) error {
	// claude-mem SQLite schema: no sync_version/sync_source columns
	rows, err := sqlite.Query(`
		SELECT memory_session_id, project, type, title, subtitle, narrative, text,
		       facts, concepts, files_read, files_modified, discovery_tokens,
		       created_at, created_at_epoch,
		       sync_id
		FROM observations
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	count, skipped := 0, 0
	for rows.Next() {
		var memSessionID, project, obsType string
		var title, subtitle, narrative, text *string
		var factsStr, conceptsStr, filesReadStr, filesModifiedStr *string
		var discoveryTokens int
		var createdAt *string
		var createdAtEpoch int64
		var syncID *string

		if err := rows.Scan(&memSessionID, &project, &obsType, &title, &subtitle, &narrative, &text,
			&factsStr, &conceptsStr, &filesReadStr, &filesModifiedStr, &discoveryTokens,
			&createdAt, &createdAtEpoch, &syncID); err != nil {
			log.Warn().Err(err).Msg("Skip observation row")
			skipped++
			continue
		}

		// Convert ms epoch to seconds
		createdAtEpoch = msToSec(createdAtEpoch)

		// Convert TEXT JSON to JSONB
		facts := toJSONB(factsStr)
		concepts := toJSONB(conceptsStr)
		filesRead := toJSONB(filesReadStr)
		filesModified := toJSONB(filesModifiedStr)

		var createdTime *time.Time
		if createdAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *createdAt); err == nil {
				createdTime = &t
			}
		}
		if createdTime == nil {
			t := time.Unix(createdAtEpoch, 0)
			createdTime = &t
		}

		_, err := pg.Exec(ctx, `
			INSERT INTO observations (memory_session_id, project, type, title, subtitle, narrative, text,
				facts, concepts, files_read, files_modified, discovery_tokens,
				created_at, created_at_epoch, sync_id, sync_version, sync_source)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, 0, NULL)
			ON CONFLICT (sync_id) DO NOTHING
		`, memSessionID, project, obsType, title, subtitle, narrative, text,
			facts, concepts, filesRead, filesModified, discoveryTokens,
			createdTime, createdAtEpoch, syncID)
		if err != nil {
			log.Warn().Err(err).Str("session", memSessionID).Msg("Failed to insert observation")
			skipped++
			continue
		}
		count++
		if count%1000 == 0 {
			log.Info().Int("count", count).Msg("Observations migrated")
		}
	}
	log.Info().Int("total", count).Int("skipped", skipped).Msg("Observations migration complete")
	return nil
}

func migrateSummaries(ctx context.Context, sqlite *sql.DB, pg *pgxpool.Pool) error {
	// claude-mem SQLite schema: no sync_version/sync_source columns
	// Multiple summaries per session are allowed (one per prompt batch)
	rows, err := sqlite.Query(`
		SELECT memory_session_id, project, request, investigated, learned, completed, next_steps,
		       notes, files_read, files_edited, discovery_tokens,
		       created_at, created_at_epoch,
		       sync_id
		FROM session_summaries
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	count, skipped := 0, 0
	for rows.Next() {
		var memSessionID, project string
		var request, investigated, learned, completed, nextSteps, notes *string
		var filesReadStr, filesEditedStr *string
		var discoveryTokens int
		var createdAt *string
		var createdAtEpoch int64
		var syncID *string

		if err := rows.Scan(&memSessionID, &project, &request, &investigated, &learned, &completed, &nextSteps,
			&notes, &filesReadStr, &filesEditedStr, &discoveryTokens,
			&createdAt, &createdAtEpoch, &syncID); err != nil {
			log.Warn().Err(err).Msg("Skip summary row")
			skipped++
			continue
		}

		// Convert ms epoch to seconds
		createdAtEpoch = msToSec(createdAtEpoch)

		filesRead := toJSONB(filesReadStr)
		filesEdited := toJSONB(filesEditedStr)

		var createdTime *time.Time
		if createdAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *createdAt); err == nil {
				createdTime = &t
			}
		}
		if createdTime == nil {
			t := time.Unix(createdAtEpoch, 0)
			createdTime = &t
		}

		// Use ON CONFLICT (sync_id) since multiple summaries per session are allowed
		_, err := pg.Exec(ctx, `
			INSERT INTO session_summaries (memory_session_id, project, request, investigated, learned, completed, next_steps,
				notes, files_read, files_edited, discovery_tokens,
				created_at, created_at_epoch, sync_id, sync_version, sync_source)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 0, NULL)
			ON CONFLICT (sync_id) DO NOTHING
		`, memSessionID, project, request, investigated, learned, completed, nextSteps,
			notes, filesRead, filesEdited, discoveryTokens,
			createdTime, createdAtEpoch, syncID)
		if err != nil {
			log.Warn().Err(err).Str("session", memSessionID).Msg("Failed to insert summary")
			skipped++
			continue
		}
		count++
		if count%1000 == 0 {
			log.Info().Int("count", count).Msg("Summaries migrated")
		}
	}
	log.Info().Int("total", count).Int("skipped", skipped).Msg("Summaries migration complete")
	return nil
}

func migratePrompts(ctx context.Context, sqlite *sql.DB, pg *pgxpool.Pool) error {
	// claude-mem SQLite schema differences:
	//   - column is prompt_text (not prompt)
	//   - no project column (must JOIN with sdk_sessions)
	//   - no sync_version/sync_source
	rows, err := sqlite.Query(`
		SELECT up.content_session_id, s.project, up.prompt_text, up.prompt_number,
		       up.created_at, up.created_at_epoch,
		       up.sync_id
		FROM user_prompts up
		JOIN sdk_sessions s ON s.content_session_id = up.content_session_id
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	count, skipped := 0, 0
	for rows.Next() {
		var contentSessionID, project, prompt string
		var promptNumber int
		var createdAt *string
		var createdAtEpoch int64
		var syncID *string

		if err := rows.Scan(&contentSessionID, &project, &prompt, &promptNumber,
			&createdAt, &createdAtEpoch, &syncID); err != nil {
			log.Warn().Err(err).Msg("Skip prompt row")
			skipped++
			continue
		}

		// Convert ms epoch to seconds
		createdAtEpoch = msToSec(createdAtEpoch)

		var createdTime *time.Time
		if createdAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *createdAt); err == nil {
				createdTime = &t
			}
		}
		if createdTime == nil {
			t := time.Unix(createdAtEpoch, 0)
			createdTime = &t
		}

		_, err := pg.Exec(ctx, `
			INSERT INTO user_prompts (content_session_id, project, prompt, prompt_number,
				created_at, created_at_epoch, sync_id, sync_version, sync_source)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 0, NULL)
			ON CONFLICT (content_session_id, prompt_number) DO NOTHING
		`, contentSessionID, project, prompt, promptNumber,
			createdTime, createdAtEpoch, syncID)
		if err != nil {
			log.Warn().Err(err).Str("session", contentSessionID).Msg("Failed to insert prompt")
			skipped++
			continue
		}
		count++
		if count%1000 == 0 {
			log.Info().Int("count", count).Msg("Prompts migrated")
		}
	}
	log.Info().Int("total", count).Int("skipped", skipped).Msg("Prompts migration complete")
	return nil
}

func backfillEmbeddings(ctx context.Context, pg *pgxpool.Pool, client *gemini.Client) error {
	log.Info().Msg("Starting embedding backfill...")

	// Observations
	if err := backfillTable(ctx, pg, client, "observations",
		`SELECT id, COALESCE(title, ''), COALESCE(subtitle, ''), COALESCE(narrative, ''), COALESCE(facts::text, '[]')
		 FROM observations WHERE embedding IS NULL ORDER BY id`,
		func(id int64, cols []string) string {
			return fmt.Sprintf("%s %s %s", cols[0], cols[1], cols[2])
		},
	); err != nil {
		return err
	}

	// Summaries
	if err := backfillTable(ctx, pg, client, "session_summaries",
		`SELECT id, COALESCE(request, ''), COALESCE(investigated, ''), COALESCE(learned, ''), COALESCE(completed, '')
		 FROM session_summaries WHERE embedding IS NULL ORDER BY id`,
		func(id int64, cols []string) string {
			return fmt.Sprintf("%s %s %s %s", cols[0], cols[1], cols[2], cols[3])
		},
	); err != nil {
		return err
	}

	// Prompts
	if err := backfillTable(ctx, pg, client, "user_prompts",
		`SELECT id, prompt, '', '', ''
		 FROM user_prompts WHERE embedding IS NULL ORDER BY id`,
		func(id int64, cols []string) string {
			return cols[0]
		},
	); err != nil {
		return err
	}

	return nil
}

func backfillTable(ctx context.Context, pg *pgxpool.Pool, client *gemini.Client, table, query string, buildText func(int64, []string) string) error {
	rows, err := pg.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("query %s: %w", table, err)
	}
	defer rows.Close()

	var ids []int64
	var texts []string
	for rows.Next() {
		var id int64
		var c1, c2, c3, c4 string
		if err := rows.Scan(&id, &c1, &c2, &c3, &c4); err != nil {
			continue
		}
		text := buildText(id, []string{c1, c2, c3, c4})
		if text == "" {
			continue
		}
		ids = append(ids, id)
		texts = append(texts, text)
	}

	if len(texts) == 0 {
		log.Info().Str("table", table).Msg("No rows to backfill")
		return nil
	}

	log.Info().Str("table", table).Int("count", len(texts)).Msg("Backfilling embeddings")

	for i := 0; i < len(texts); i += 100 {
		end := i + 100
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]
		embeddings, err := client.EmbedBatch(ctx, batch)
		if err != nil {
			log.Warn().Err(err).Int("batch_start", i).Msg("Batch embed failed")
			continue
		}

		for j, emb := range embeddings {
			v := pgvector.NewVector(emb)
			pg.Exec(ctx, fmt.Sprintf(`UPDATE %s SET embedding = $1 WHERE id = $2`, table), &v, ids[i+j])
		}
		log.Info().Str("table", table).Int("progress", end).Int("total", len(texts)).Msg("Embedding progress")
	}
	return nil
}

func verify(ctx context.Context, sqlite *sql.DB, pg *pgxpool.Pool) error {
	tables := []string{"sdk_sessions", "observations", "session_summaries", "user_prompts"}

	log.Info().Msg("Verifying migration...")
	for _, table := range tables {
		var sqliteCount, pgCount int
		sqlite.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&sqliteCount)
		pg.QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&pgCount)

		if sqliteCount != pgCount {
			log.Warn().Str("table", table).Int("sqlite", sqliteCount).Int("postgres", pgCount).Msg("Row count mismatch")
		} else {
			log.Info().Str("table", table).Int("count", pgCount).Msg("Row count matches")
		}
	}

	var nullEmbeddings int
	pg.QueryRow(ctx, "SELECT COUNT(*) FROM observations WHERE embedding IS NULL").Scan(&nullEmbeddings)
	if nullEmbeddings > 0 {
		log.Warn().Int("count", nullEmbeddings).Msg("Observations missing embeddings (run with --gemini-key to backfill)")
	}

	log.Info().Msg("Migration verification complete")
	return nil
}

// toJSONB converts a nullable string (JSON text) to []byte for JSONB insertion.
func toJSONB(s *string) []byte {
	if s == nil || *s == "" {
		return []byte("[]")
	}
	// Validate it's valid JSON
	var v json.RawMessage
	if err := json.Unmarshal([]byte(*s), &v); err != nil {
		return []byte("[]")
	}
	return []byte(*s)
}
