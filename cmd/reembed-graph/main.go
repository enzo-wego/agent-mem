package main

import (
	"context"
	"os"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	apiKey := os.Getenv("AGENT_MEM_GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if dsn == "" || apiKey == "" {
		log.Fatal().Msg("DATABASE_URL and AGENT_MEM_GEMINI_API_KEY (OpenRouter key) are required")
	}

	pg, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("connect pg")
	}
	defer pg.Close()

	// 3072-dim client, no task_type — matches the worker's graph query embeddings.
	// Provider follows AGENT_MEM_LLM_PROVIDER (default openrouter) so a re-embed can
	// run against either backend; vectors are compatible either way.
	client := gemini.NewClient(os.Getenv("AGENT_MEM_LLM_PROVIDER"), apiKey, "", "google/gemini-embedding-001", 3072)

	rows, err := pg.Query(ctx, `SELECT node_id, summary FROM graph.artifact_index WHERE summary <> '' ORDER BY node_id`)
	if err != nil {
		log.Fatal().Err(err).Msg("select artifact_index")
	}
	var ids, texts []string
	for rows.Next() {
		var id, summary string
		if err := rows.Scan(&id, &summary); err != nil {
			continue
		}
		ids = append(ids, id)
		texts = append(texts, summary)
	}
	rows.Close()

	log.Info().Int("count", len(texts)).Msg("Re-embedding graph.artifact_index via OpenRouter")

	for i := 0; i < len(texts); i += 100 {
		end := i + 100
		if end > len(texts) {
			end = len(texts)
		}
		embeddings, err := client.EmbedBatch(ctx, texts[i:end])
		if err != nil {
			log.Warn().Err(err).Int("batch_start", i).Msg("Batch embed failed; skipping batch")
			continue
		}
		for j, emb := range embeddings {
			v := pgvector.NewVector(emb)
			if _, err := pg.Exec(ctx,
				`UPDATE graph.artifact_index SET embedding = $1, refreshed_at = NOW() WHERE node_id = $2`,
				&v, ids[i+j]); err != nil {
				log.Warn().Err(err).Str("node_id", ids[i+j]).Msg("update failed")
			}
		}
		log.Info().Int("progress", end).Int("total", len(texts)).Msg("progress")
	}
	log.Info().Msg("Done")
}
