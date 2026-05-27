// Package identity manages graph.people rows and identity merging.
// It lazily creates person rows from inbound source references and merges
// duplicate identities when later evidence (e-mail) arrives.
package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// Service manages graph.people and graph.identity_map rows.
type Service struct {
	db  *pgxpool.Pool
	log zerolog.Logger
}

// NewService creates a new identity Service.
func NewService(db *pgxpool.Pool, log zerolog.Logger) *Service {
	return &Service{db: db, log: log}
}

// Ref describes a single inbound identity reference.
// Source is one of "slack","jira","github","confluence","pagerduty","datadog","sentry","gws".
// ExternalID is the source-native identifier.
type Ref struct {
	Source      string
	ExternalID  string
	DisplayName string
	Email       string // may be empty if not yet known
	IsBot       bool
}

// sourceColumn maps a source name to its unique column in graph.people.
// Returns "" for sources that rely on email-only dedup.
func sourceColumn(source string) string {
	switch source {
	case "slack":
		return "slack_user_id"
	case "jira", "confluence":
		return "jira_account_id"
	case "github":
		return "github_login"
	case "pagerduty":
		return "pagerduty_user_id"
	default:
		// datadog, sentry, gws — email-only
		return ""
	}
}

// EnsurePerson upserts the person row and returns its primary key (BIGINT).
//
// Logic:
//  1. If identity_map has (source, external_id) -> use that person_id.
//  2. Else if Email is non-empty and matches an existing graph.people.email
//     -> use that person_id; insert identity_map row.
//  3. Else INSERT a new graph.people row. Insert the identity_map row.
//  4. If Email was provided and the existing row has NULL email, UPDATE.
//  5. Always refresh display_name.
//
// Returns (personID, created bool, err).
func (s *Service) EnsurePerson(ctx context.Context, r Ref) (personID int64, created bool, err error) {
	// Step 1: check identity_map
	var mapPersonID int64
	err = s.db.QueryRow(ctx,
		`SELECT person_id FROM graph.identity_map WHERE source = $1 AND external_id = $2`,
		r.Source, r.ExternalID,
	).Scan(&mapPersonID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("identity_map lookup: %w", err)
	}
	if err == nil {
		// Found in identity_map; refresh display_name and optionally fill email.
		personID = mapPersonID
		if err2 := s.refreshPerson(ctx, personID, r); err2 != nil {
			return 0, false, err2
		}
		return personID, false, nil
	}

	// Step 2: look up by email if provided
	if r.Email != "" {
		var emailPersonID int64
		err = s.db.QueryRow(ctx,
			`SELECT id FROM graph.people WHERE email = $1 AND merged_into IS NULL`,
			r.Email,
		).Scan(&emailPersonID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, false, fmt.Errorf("people email lookup: %w", err)
		}
		if err == nil {
			// Bind to existing row via identity_map.
			if err2 := s.insertIdentityMap(ctx, r.Source, r.ExternalID, emailPersonID); err2 != nil {
				return 0, false, err2
			}
			if err2 := s.refreshPerson(ctx, emailPersonID, r); err2 != nil {
				return 0, false, err2
			}
			return emailPersonID, false, nil
		}
	}

	// Step 3: insert new graph.people row.
	col := sourceColumn(r.Source)
	var newID int64
	if col != "" {
		query := fmt.Sprintf(`
			INSERT INTO graph.people (display_name, email, %s, is_bot, machine_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (%s) DO UPDATE
			  SET display_name = EXCLUDED.display_name,
			      email        = COALESCE(graph.people.email, EXCLUDED.email)
			RETURNING id`, col, col)
		err = s.db.QueryRow(ctx, query,
			r.DisplayName, nullableString(r.Email), r.ExternalID, r.IsBot, "local",
		).Scan(&newID)
	} else {
		// email-only sources: upsert on email (or plain insert if no email)
		if r.Email != "" {
			err = s.db.QueryRow(ctx, `
				INSERT INTO graph.people (display_name, email, is_bot, machine_id)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (email) DO UPDATE
				  SET display_name = EXCLUDED.display_name
				RETURNING id`,
				r.DisplayName, r.Email, r.IsBot, "local",
			).Scan(&newID)
		} else {
			err = s.db.QueryRow(ctx, `
				INSERT INTO graph.people (display_name, is_bot, machine_id)
				VALUES ($1, $2, $3)
				RETURNING id`,
				r.DisplayName, r.IsBot, "local",
			).Scan(&newID)
		}
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert person: %w", err)
	}

	// Insert identity_map row (ignore conflict — the ON CONFLICT above may have
	// returned an existing row's id if the source column matched).
	if err2 := s.insertIdentityMap(ctx, r.Source, r.ExternalID, newID); err2 != nil {
		return 0, false, err2
	}

	return newID, true, nil
}

// MergeByEmail consolidates two graph.people rows that share the same email.
// The canonical row (the one with eeid set, or the older row) wins; the other
// gets merged_into = canonical_id. All author_person_id and identity_map
// references are rewritten. Idempotent. Returns canonical id.
func (s *Service) MergeByEmail(ctx context.Context, email string) (canonicalID int64, err error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, eeid FROM graph.people WHERE email = $1 AND merged_into IS NULL ORDER BY id`,
		email,
	)
	if err != nil {
		return 0, fmt.Errorf("MergeByEmail query: %w", err)
	}
	type row struct {
		id   int64
		eeid *int
	}
	var people []row
	for rows.Next() {
		var r row
		if err2 := rows.Scan(&r.id, &r.eeid); err2 != nil {
			rows.Close()
			return 0, fmt.Errorf("MergeByEmail scan: %w", err2)
		}
		people = append(people, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, fmt.Errorf("MergeByEmail rows: %w", err)
	}

	if len(people) <= 1 {
		if len(people) == 1 {
			return people[0].id, nil
		}
		return 0, fmt.Errorf("MergeByEmail: no people row found for email %q", email)
	}

	// Choose canonical: prefer the row with a non-nil eeid; otherwise the oldest (lowest id).
	canonical := people[0]
	for _, p := range people[1:] {
		if p.eeid != nil && canonical.eeid == nil {
			canonical = p
		}
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("MergeByEmail begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	for _, p := range people {
		if p.id == canonical.id {
			continue
		}
		// Rewrite graph.nodes references.
		if _, err = tx.Exec(ctx,
			`UPDATE graph.nodes SET author_person_id = $1 WHERE author_person_id = $2`,
			canonical.id, p.id,
		); err != nil {
			return 0, fmt.Errorf("MergeByEmail rewrite nodes: %w", err)
		}
		// Rewrite identity_map references.
		if _, err = tx.Exec(ctx,
			`UPDATE graph.identity_map SET person_id = $1 WHERE person_id = $2`,
			canonical.id, p.id,
		); err != nil {
			return 0, fmt.Errorf("MergeByEmail rewrite identity_map: %w", err)
		}
		// Soft-delete the duplicate.
		if _, err = tx.Exec(ctx,
			`UPDATE graph.people SET merged_into = $1 WHERE id = $2`,
			canonical.id, p.id,
		); err != nil {
			return 0, fmt.Errorf("MergeByEmail soft-delete: %w", err)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("MergeByEmail commit: %w", err)
	}
	return canonical.id, nil
}

// Resolve follows the chain of merged_into pointers and returns the canonical person_id.
func (s *Service) Resolve(ctx context.Context, personID int64) (int64, error) {
	seen := map[int64]bool{personID: true}
	current := personID
	for {
		var mergedInto *int64
		err := s.db.QueryRow(ctx,
			`SELECT merged_into FROM graph.people WHERE id = $1`,
			current,
		).Scan(&mergedInto)
		if err != nil {
			return 0, fmt.Errorf("Resolve person %d: %w", current, err)
		}
		if mergedInto == nil {
			return current, nil
		}
		if seen[*mergedInto] {
			return 0, fmt.Errorf("Resolve: cycle detected at person_id %d", *mergedInto)
		}
		seen[*mergedInto] = true
		current = *mergedInto
	}
}

// refreshPerson updates display_name and fills in email if the existing row has NULL email.
func (s *Service) refreshPerson(ctx context.Context, personID int64, r Ref) error {
	if r.Email != "" {
		_, err := s.db.Exec(ctx, `
			UPDATE graph.people
			SET display_name = $2,
			    email        = COALESCE(email, $3)
			WHERE id = $1`,
			personID, r.DisplayName, r.Email,
		)
		if err != nil {
			return fmt.Errorf("refreshPerson (with email): %w", err)
		}
		return nil
	}
	_, err := s.db.Exec(ctx, `
		UPDATE graph.people SET display_name = $2 WHERE id = $1`,
		personID, r.DisplayName,
	)
	if err != nil {
		return fmt.Errorf("refreshPerson: %w", err)
	}
	return nil
}

// insertIdentityMap inserts a row into graph.identity_map, ignoring conflicts.
func (s *Service) insertIdentityMap(ctx context.Context, source, externalID string, personID int64) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO graph.identity_map (source, external_id, person_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (source, external_id) DO UPDATE SET person_id = EXCLUDED.person_id`,
		source, externalID, personID,
	)
	if err != nil {
		return fmt.Errorf("insertIdentityMap: %w", err)
	}
	return nil
}

// nullableString converts an empty string to nil for nullable TEXT columns.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
