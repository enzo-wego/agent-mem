package database

import (
	"context"
	"fmt"
	"time"
)

// LLMKeyBlock is one key taken out of rotation. Fingerprint (sha256 prefix) and
// KeyTail identify the key without storing the secret.
type LLMKeyBlock struct {
	Fingerprint string     `json:"fingerprint"`
	KeyTail     string     `json:"key_tail"`
	Provider    string     `json:"provider"`
	Reason      string     `json:"reason"`
	BlockedAt   time.Time  `json:"blocked_at"`
	ExpiresAt   *time.Time `json:"expires_at"` // nil = permanent, until manually unblocked
}

// BlockLLMKey records (or refreshes) a block. A zero until means permanent.
func (db *DB) BlockLLMKey(ctx context.Context, fingerprint, keyTail, provider, reason string, until time.Time) error {
	var expires *time.Time
	if !until.IsZero() {
		expires = &until
	}
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO llm_key_blocks (fingerprint, key_tail, provider, reason, blocked_at, expires_at)
		VALUES ($1, $2, $3, $4, now(), $5)
		ON CONFLICT (fingerprint) DO UPDATE SET
			key_tail = EXCLUDED.key_tail,
			provider = EXCLUDED.provider,
			reason = EXCLUDED.reason,
			blocked_at = now(),
			expires_at = EXCLUDED.expires_at
	`, fingerprint, keyTail, provider, reason, expires)
	if err != nil {
		return fmt.Errorf("block llm key %s: %w", fingerprint, err)
	}
	return nil
}

// ListLLMKeyBlocks returns all blocks, newest first, including expired ones so
// the dashboard can show recent history.
func (db *DB) ListLLMKeyBlocks(ctx context.Context) ([]LLMKeyBlock, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT fingerprint, key_tail, provider, reason, blocked_at, expires_at
		FROM llm_key_blocks ORDER BY blocked_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list llm key blocks: %w", err)
	}
	defer rows.Close()

	out := []LLMKeyBlock{}
	for rows.Next() {
		var b LLMKeyBlock
		if err := rows.Scan(&b.Fingerprint, &b.KeyTail, &b.Provider, &b.Reason, &b.BlockedAt, &b.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan llm key block: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ActiveLLMKeyBlocks returns the fingerprints still blocked right now — what the
// gemini client is seeded with at startup.
func (db *DB) ActiveLLMKeyBlocks(ctx context.Context) ([]string, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT fingerprint FROM llm_key_blocks
		WHERE expires_at IS NULL OR expires_at > now()
	`)
	if err != nil {
		return nil, fmt.Errorf("active llm key blocks: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scan llm key block fingerprint: %w", err)
		}
		out = append(out, fp)
	}
	return out, rows.Err()
}

// UnblockLLMKey drops a block (dashboard "unblock" / key replaced).
func (db *DB) UnblockLLMKey(ctx context.Context, fingerprint string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM llm_key_blocks WHERE fingerprint = $1`, fingerprint)
	if err != nil {
		return fmt.Errorf("unblock llm key %s: %w", fingerprint, err)
	}
	return nil
}
