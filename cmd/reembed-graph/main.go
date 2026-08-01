package main

import (
	"context"
	"os"
	"time"

	"github.com/agent-mem/agent-mem/internal/llmgateway"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	gwURL := os.Getenv("LLM_GATEWAY_URL")
	gwKey := os.Getenv("LLM_GATEWAY_API_KEY")
	if dsn == "" || gwURL == "" || gwKey == "" {
		log.Fatal().Msg("DATABASE_URL, LLM_GATEWAY_URL and LLM_GATEWAY_API_KEY are required")
	}

	pg, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("connect pg")
	}
	defer pg.Close()

	// 3072 dims to match graph.artifact_index.embedding, halfvec(3072). Goes
	// through llm-gateway like every other LLM call — this tool holds no provider
	// credentials of its own. The gateway serves embeddings from OpenRouter, so
	// the vectors stay in the same space as the ones already stored.
	client := llmgateway.New(gwURL, gwKey, 3072)

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

	log.Info().Int("count", len(texts)).Msg("Re-embedding graph.artifact_index via llm-gateway")

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
