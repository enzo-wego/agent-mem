package database

import (
	"context"
	"fmt"
)

// GetAllSettings returns all settings as a key-value map.
func (db *DB) GetAllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := db.Pool.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("get all settings: %w", err)
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		settings[k] = v
	}
	return settings, rows.Err()
}

// SaveSetting upserts a single setting.
func (db *DB) SaveSetting(ctx context.Context, key, value string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("save setting %s: %w", key, err)
	}
	return nil
}

// SaveSettings upserts multiple settings.
func (db *DB) SaveSettings(ctx context.Context, settings map[string]string) error {
	for k, v := range settings {
		if err := db.SaveSetting(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}
