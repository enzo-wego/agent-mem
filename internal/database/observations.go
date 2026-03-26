package database

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pgvector/pgvector-go"
)

// StoreObservation inserts an observation with its embedding into PostgreSQL.
func (db *DB) StoreObservation(ctx context.Context, memorySessionID, project, obsType, title, subtitle, narrative string, facts, concepts, filesRead, filesModified []string, embedding []float32) (int64, error) {
	now := time.Now()
	epoch := now.Unix()

	factsJSON, _ := json.Marshal(facts)
	conceptsJSON, _ := json.Marshal(concepts)
	filesReadJSON, _ := json.Marshal(filesRead)
	filesModifiedJSON, _ := json.Marshal(filesModified)

	var embeddingVec *pgvector.Vector
	if len(embedding) > 0 {
		v := pgvector.NewVector(embedding)
		embeddingVec = &v
	}

	var id int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO observations (
			memory_session_id, project, type, title, subtitle, narrative,
			facts, concepts, files_read, files_modified,
			created_at, created_at_epoch, embedding
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id
	`,
		memorySessionID, project, obsType, title, subtitle, narrative,
		factsJSON, conceptsJSON, filesReadJSON, filesModifiedJSON,
		now, epoch, embeddingVec,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store observation: %w", err)
	}
	return id, nil
}

// StoreSummary inserts a session summary with its embedding into PostgreSQL.
func (db *DB) StoreSummary(ctx context.Context, memorySessionID, project, request, investigated, learned, completed, nextSteps string, embedding []float32) (int64, error) {
	now := time.Now()
	epoch := now.Unix()

	var embeddingVec *pgvector.Vector
	if len(embedding) > 0 {
		v := pgvector.NewVector(embedding)
		embeddingVec = &v
	}

	var id int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO session_summaries (
			memory_session_id, project, request, investigated, learned, completed, next_steps,
			created_at, created_at_epoch, embedding
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`,
		memorySessionID, project, request, investigated, learned, completed, nextSteps,
		now, epoch, embeddingVec,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store summary: %w", err)
	}
	return id, nil
}

// UpdatePromptEmbedding updates the embedding for a stored user prompt.
func (db *DB) UpdatePromptEmbedding(ctx context.Context, promptID int64, embedding []float32) error {
	v := pgvector.NewVector(embedding)
	_, err := db.Pool.Exec(ctx, `
		UPDATE user_prompts SET embedding = $2 WHERE id = $1
	`, promptID, &v)
	if err != nil {
		return fmt.Errorf("update prompt embedding: %w", err)
	}
	return nil
}
