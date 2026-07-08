package database

import (
	"context"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Syncable graph types
// ---------------------------------------------------------------------------

// SyncableGraphNode is a graph.nodes row for sync transport.
type SyncableGraphNode struct {
	ID             string     `json:"id"`
	Type           string     `json:"type"`
	NaturalKey     string     `json:"natural_key"`
	URL            *string    `json:"url,omitempty"`
	Title          *string    `json:"title,omitempty"`
	Body           *string    `json:"body,omitempty"`
	BodyRevision   int        `json:"body_revision"`
	BodyTS         *time.Time `json:"body_ts,omitempty"`
	MimeType       *string    `json:"mime_type,omitempty"`
	SizeBytes      *int64     `json:"size_bytes,omitempty"`
	ExternalURL    *string    `json:"external_url,omitempty"`
	ThumbURL       *string    `json:"thumb_url,omitempty"`
	AuthorPersonID *int64     `json:"author_person_id,omitempty"`
	Scope          *string    `json:"scope,omitempty"`
	Metadata       []byte     `json:"metadata"`
	FirstSeenAt    time.Time  `json:"first_seen_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
	SyncID         string     `json:"sync_id"`
	SyncVersion    int64      `json:"sync_version"`
	MachineID      string     `json:"machine_id"`
}

// SyncableGraphEdge is a graph.edges row for sync transport.
type SyncableGraphEdge struct {
	ID           int64     `json:"id"`
	FromNodeID   string    `json:"from_node_id"`
	ToNodeID     string    `json:"to_node_id"`
	Kind         string    `json:"kind"`
	SourceMsgID  *string   `json:"source_msg_id,omitempty"`
	BodyRevision int       `json:"body_revision"`
	Metadata     []byte    `json:"metadata"`
	CreatedAt    time.Time `json:"created_at"`
	SyncID       string    `json:"sync_id"`
	SyncVersion  int64     `json:"sync_version"`
	MachineID    string    `json:"machine_id"`
}

// SyncableGraphPerson is a graph.people row for sync transport.
type SyncableGraphPerson struct {
	ID                 int64      `json:"id"`
	EEID               *int       `json:"eeid,omitempty"`
	Email              *string    `json:"email,omitempty"`
	DisplayName        string     `json:"display_name"`
	SlackUserID        *string    `json:"slack_user_id,omitempty"`
	JiraAccountID      *string    `json:"jira_account_id,omitempty"`
	GithubLogin        *string    `json:"github_login,omitempty"`
	PagerdutyUserID    *string    `json:"pagerduty_user_id,omitempty"`
	IsBot              bool       `json:"is_bot"`
	ReportsTo          *int       `json:"reports_to,omitempty"`
	DepthFromRoot      *int16     `json:"depth_from_root,omitempty"`
	FirstSeenAt        time.Time  `json:"first_seen_at"`
	IdentityResolvedAt *time.Time `json:"identity_resolved_at,omitempty"`
	MergedInto         *int64     `json:"merged_into,omitempty"`
	SyncID             string     `json:"sync_id"`
	SyncVersion        int64      `json:"sync_version"`
	MachineID          string     `json:"machine_id"`
}

// SyncableGraphArtifactIndex is a graph.artifact_index row for sync transport.
type SyncableGraphArtifactIndex struct {
	NodeID      string    `json:"node_id"`
	Summary     *string   `json:"summary,omitempty"`
	SummaryKind string    `json:"summary_kind"`
	Embedding   []float32 `json:"embedding,omitempty"`
	RefreshedAt time.Time `json:"refreshed_at"`
	SyncID      string    `json:"sync_id"`
	SyncVersion int64     `json:"sync_version"`
	MachineID   string    `json:"machine_id"`
}

// SyncableGraphArtifactBody is a graph.artifact_bodies row for sync transport.
type SyncableGraphArtifactBody struct {
	NodeID      string     `json:"node_id"`
	BodyFull    string     `json:"body_full"`
	OCRText     *string    `json:"ocr_text,omitempty"`
	Description *string    `json:"description,omitempty"`
	FetchedAt   time.Time  `json:"fetched_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	SyncID      string     `json:"sync_id"`
	SyncVersion int64      `json:"sync_version"`
	MachineID   string     `json:"machine_id"`
}

// SyncableGraphSlackGroup is a graph.slack_groups row for sync transport.
type SyncableGraphSlackGroup struct {
	ID            string    `json:"id"`
	Handle        string    `json:"handle"`
	Name          string    `json:"name"`
	Description   *string   `json:"description,omitempty"`
	MemberUserIDs []string  `json:"member_user_ids"`
	UserCount     int       `json:"user_count"`
	RefreshedAt   time.Time `json:"refreshed_at"`
	SyncID        string    `json:"sync_id"`
	SyncVersion   int64     `json:"sync_version"`
	MachineID     string    `json:"machine_id"`
}

// SyncableGraphEntity is a graph.entities row for sync transport.
type SyncableGraphEntity struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	DisplayName string    `json:"display_name"`
	Aliases     []string  `json:"aliases"`
	Metadata    []byte    `json:"metadata"`
	Source      string    `json:"source"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	SyncID      string    `json:"sync_id"`
	SyncVersion int64     `json:"sync_version"`
	MachineID   string    `json:"machine_id"`
}

// SyncableGraphIdentityMap is a graph.identity_map row for sync transport.
// identity_map has no sync_id/sync_version — we sync it via the parent people row.
// But we include it for completeness as a simple upsert-on-conflict approach.
type SyncableGraphIdentityMap struct {
	Source     string    `json:"source"`
	ExternalID string    `json:"external_id"`
	PersonID   int64     `json:"person_id"`
	ResolvedAt time.Time `json:"resolved_at"`
}

// SyncableGraphJob is a graph.jobs row for sync transport (target_runner != 'local' only).
type SyncableGraphJob struct {
	ID           int64      `json:"id"`
	Type         string     `json:"type"`
	Payload      []byte     `json:"payload"`
	Priority     int16      `json:"priority"`
	Status       string     `json:"status"`
	AvailableAt  time.Time  `json:"available_at"`
	Attempts     int16      `json:"attempts"`
	MaxAttempts  int16      `json:"max_attempts"`
	LastError    *string    `json:"last_error,omitempty"`
	LockedBy     *string    `json:"locked_by,omitempty"`
	LockedAt     *time.Time `json:"locked_at,omitempty"`
	EnqueuedAt   time.Time  `json:"enqueued_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	TargetRunner string     `json:"target_runner"`
	SyncID       string     `json:"sync_id"`
	SyncVersion  int64      `json:"sync_version"`
	MachineID    string     `json:"machine_id"`
}

// SyncableGraphUserAffinityConfig is a graph.user_affinity_config row for sync transport.
type SyncableGraphUserAffinityConfig struct {
	EEID                int       `json:"eeid"`
	TeamGroupIDs        []string  `json:"team_group_ids"`
	DeptGroupIDs        []string  `json:"dept_group_ids"`
	TeamSubtreeRootEEID *int      `json:"team_subtree_root_eeid,omitempty"`
	Autodetected        bool      `json:"autodetected"`
	UpdatedAt           time.Time `json:"updated_at"`
	SyncID              string    `json:"sync_id"`
	SyncVersion         int64     `json:"sync_version"`
	MachineID           string    `json:"machine_id"`
}

// ---------------------------------------------------------------------------
// GetUnsynced* — collect rows with sync_version = 0 for push
// ---------------------------------------------------------------------------

// GetUnsyncedGraphPeople returns graph.people rows not yet synced.
func (db *DB) GetUnsyncedGraphPeople(ctx context.Context, limit int) ([]SyncableGraphPerson, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, eeid, email, display_name, slack_user_id, jira_account_id,
		       github_login, pagerduty_user_id, is_bot, reports_to, depth_from_root,
		       first_seen_at, identity_resolved_at, merged_into,
		       sync_id::text, sync_version, machine_id
		FROM graph.people
		WHERE sync_version = 0
		ORDER BY id ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced graph people: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphPerson
	for rows.Next() {
		var p SyncableGraphPerson
		if err := rows.Scan(
			&p.ID, &p.EEID, &p.Email, &p.DisplayName, &p.SlackUserID, &p.JiraAccountID,
			&p.GithubLogin, &p.PagerdutyUserID, &p.IsBot, &p.ReportsTo, &p.DepthFromRoot,
			&p.FirstSeenAt, &p.IdentityResolvedAt, &p.MergedInto,
			&p.SyncID, &p.SyncVersion, &p.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph person: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetUnsyncedGraphNodes returns graph.nodes rows not yet synced.
func (db *DB) GetUnsyncedGraphNodes(ctx context.Context, limit int) ([]SyncableGraphNode, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, type, natural_key, url, title, body, body_revision, body_ts,
		       mime_type, size_bytes, external_url, thumb_url,
		       author_person_id, scope, metadata, first_seen_at, updated_at, deleted_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.nodes
		WHERE sync_version = 0
		ORDER BY updated_at ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced graph nodes: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphNode
	for rows.Next() {
		var n SyncableGraphNode
		if err := rows.Scan(
			&n.ID, &n.Type, &n.NaturalKey, &n.URL, &n.Title, &n.Body, &n.BodyRevision, &n.BodyTS,
			&n.MimeType, &n.SizeBytes, &n.ExternalURL, &n.ThumbURL,
			&n.AuthorPersonID, &n.Scope, &n.Metadata, &n.FirstSeenAt, &n.UpdatedAt, &n.DeletedAt,
			&n.SyncID, &n.SyncVersion, &n.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph node: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetUnsyncedGraphEdges returns graph.edges rows not yet synced.
func (db *DB) GetUnsyncedGraphEdges(ctx context.Context, limit int) ([]SyncableGraphEdge, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, from_node_id, to_node_id, kind, source_msg_id, body_revision, metadata, created_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.edges
		WHERE sync_version = 0
		ORDER BY id ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced graph edges: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphEdge
	for rows.Next() {
		var e SyncableGraphEdge
		if err := rows.Scan(
			&e.ID, &e.FromNodeID, &e.ToNodeID, &e.Kind, &e.SourceMsgID, &e.BodyRevision, &e.Metadata, &e.CreatedAt,
			&e.SyncID, &e.SyncVersion, &e.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph edge: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetUnsyncedGraphArtifactIndex returns graph.artifact_index rows not yet synced.
func (db *DB) GetUnsyncedGraphArtifactIndex(ctx context.Context, limit int) ([]SyncableGraphArtifactIndex, error) {
	// Note: embedding column is excluded from sync transport to keep payloads small.
	// The receiving machine re-generates embeddings via the index_artifact job.
	rows, err := db.Pool.Query(ctx, `
		SELECT node_id, summary, summary_kind, refreshed_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.artifact_index
		WHERE sync_version = 0
		ORDER BY refreshed_at ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced graph artifact_index: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphArtifactIndex
	for rows.Next() {
		var ai SyncableGraphArtifactIndex
		if err := rows.Scan(
			&ai.NodeID, &ai.Summary, &ai.SummaryKind, &ai.RefreshedAt,
			&ai.SyncID, &ai.SyncVersion, &ai.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph artifact_index: %w", err)
		}
		out = append(out, ai)
	}
	return out, rows.Err()
}

// GetUnsyncedGraphArtifactBodies returns graph.artifact_bodies rows not yet synced.
func (db *DB) GetUnsyncedGraphArtifactBodies(ctx context.Context, limit int) ([]SyncableGraphArtifactBody, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT node_id, body_full, ocr_text, description, fetched_at, expires_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.artifact_bodies
		WHERE sync_version = 0
		ORDER BY fetched_at ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced graph artifact_bodies: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphArtifactBody
	for rows.Next() {
		var ab SyncableGraphArtifactBody
		if err := rows.Scan(
			&ab.NodeID, &ab.BodyFull, &ab.OCRText, &ab.Description, &ab.FetchedAt, &ab.ExpiresAt,
			&ab.SyncID, &ab.SyncVersion, &ab.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph artifact_body: %w", err)
		}
		out = append(out, ab)
	}
	return out, rows.Err()
}

// GetUnsyncedGraphSlackGroups returns graph.slack_groups rows not yet synced.
func (db *DB) GetUnsyncedGraphSlackGroups(ctx context.Context, limit int) ([]SyncableGraphSlackGroup, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, handle, name, description, member_user_ids, user_count, refreshed_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.slack_groups
		WHERE sync_version = 0
		ORDER BY id ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced graph slack_groups: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphSlackGroup
	for rows.Next() {
		var sg SyncableGraphSlackGroup
		if err := rows.Scan(
			&sg.ID, &sg.Handle, &sg.Name, &sg.Description, &sg.MemberUserIDs, &sg.UserCount, &sg.RefreshedAt,
			&sg.SyncID, &sg.SyncVersion, &sg.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph slack_group: %w", err)
		}
		out = append(out, sg)
	}
	return out, rows.Err()
}

// GetUnsyncedGraphEntities returns graph.entities rows not yet synced.
func (db *DB) GetUnsyncedGraphEntities(ctx context.Context, limit int) ([]SyncableGraphEntity, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, kind, display_name, aliases, metadata, source, first_seen_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.entities
		WHERE sync_version = 0
		ORDER BY id ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced graph entities: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphEntity
	for rows.Next() {
		var e SyncableGraphEntity
		if err := rows.Scan(
			&e.ID, &e.Kind, &e.DisplayName, &e.Aliases, &e.Metadata, &e.Source, &e.FirstSeenAt,
			&e.SyncID, &e.SyncVersion, &e.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph entity: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetUnsyncedGraphJobs returns graph.jobs rows (target_runner != 'local') not yet synced.
func (db *DB) GetUnsyncedGraphJobs(ctx context.Context, limit int) ([]SyncableGraphJob, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, type, payload, priority, status, available_at, attempts, max_attempts,
		       last_error, locked_by, locked_at, enqueued_at, completed_at, target_runner,
		       sync_id::text, sync_version, machine_id
		FROM graph.jobs
		WHERE sync_version = 0 AND target_runner != 'local'
		ORDER BY id ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced graph jobs: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphJob
	for rows.Next() {
		var j SyncableGraphJob
		if err := rows.Scan(
			&j.ID, &j.Type, &j.Payload, &j.Priority, &j.Status, &j.AvailableAt, &j.Attempts, &j.MaxAttempts,
			&j.LastError, &j.LockedBy, &j.LockedAt, &j.EnqueuedAt, &j.CompletedAt, &j.TargetRunner,
			&j.SyncID, &j.SyncVersion, &j.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// GetUnsyncedGraphUserAffinityConfig returns graph.user_affinity_config rows not yet synced.
func (db *DB) GetUnsyncedGraphUserAffinityConfig(ctx context.Context, limit int) ([]SyncableGraphUserAffinityConfig, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT eeid, team_group_ids, dept_group_ids, team_subtree_root_eeid,
		       autodetected, updated_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.user_affinity_config
		WHERE sync_version = 0
		ORDER BY eeid ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced graph user_affinity_config: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphUserAffinityConfig
	for rows.Next() {
		var c SyncableGraphUserAffinityConfig
		if err := rows.Scan(
			&c.EEID, &c.TeamGroupIDs, &c.DeptGroupIDs, &c.TeamSubtreeRootEEID,
			&c.Autodetected, &c.UpdatedAt,
			&c.SyncID, &c.SyncVersion, &c.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph user_affinity_config: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// MarkSyncedGraph* — mark rows as synced by sync_id
// ---------------------------------------------------------------------------

// MarkSyncedGraphBySyncID updates sync_version for graph table rows identified by sync_id strings.
func (db *DB) MarkSyncedGraphBySyncID(ctx context.Context, schema, table string, syncIDs []string, version int64) error {
	if len(syncIDs) == 0 {
		return nil
	}
	q := fmt.Sprintf(`UPDATE %s.%s SET sync_version = $1 WHERE sync_id::text = ANY($2)`, schema, table)
	_, err := db.Pool.Exec(ctx, q, version, syncIDs)
	if err != nil {
		return fmt.Errorf("mark synced graph %s.%s: %w", schema, table, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Import* — receive rows from sync (ON CONFLICT (sync_id) DO NOTHING)
// ---------------------------------------------------------------------------

// ImportGraphPerson imports a graph.people row from sync.
func (db *DB) ImportGraphPerson(ctx context.Context, p *SyncableGraphPerson) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.people
			(eeid, email, display_name, slack_user_id, jira_account_id,
			 github_login, pagerduty_user_id, is_bot, reports_to, depth_from_root,
			 first_seen_at, identity_resolved_at, merged_into,
			 sync_id, sync_version, machine_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (sync_id) DO NOTHING`,
		p.EEID, p.Email, p.DisplayName, p.SlackUserID, p.JiraAccountID,
		p.GithubLogin, p.PagerdutyUserID, p.IsBot, p.ReportsTo, p.DepthFromRoot,
		p.FirstSeenAt, p.IdentityResolvedAt, p.MergedInto,
		p.SyncID, p.SyncVersion, p.MachineID,
	)
	return err
}

// ImportGraphNode imports a graph.nodes row from sync.
func (db *DB) ImportGraphNode(ctx context.Context, n *SyncableGraphNode) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.nodes
			(id, type, natural_key, url, title, body, body_revision, body_ts,
			 mime_type, size_bytes, external_url, thumb_url,
			 author_person_id, scope, metadata, first_seen_at, updated_at, deleted_at,
			 sync_id, sync_version, machine_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (sync_id) DO NOTHING`,
		n.ID, n.Type, n.NaturalKey, n.URL, n.Title, n.Body, n.BodyRevision, n.BodyTS,
		n.MimeType, n.SizeBytes, n.ExternalURL, n.ThumbURL,
		n.AuthorPersonID, n.Scope, n.Metadata, n.FirstSeenAt, n.UpdatedAt, n.DeletedAt,
		n.SyncID, n.SyncVersion, n.MachineID,
	)
	return err
}

// ImportGraphEdge imports a graph.edges row from sync.
// Skips if referenced nodes don't exist locally (FK constraint).
func (db *DB) ImportGraphEdge(ctx context.Context, e *SyncableGraphEdge) error {
	metadata := string(e.Metadata)
	if metadata == "" {
		metadata = "{}"
	}
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.edges
			(from_node_id, to_node_id, kind, source_msg_id, body_revision, metadata, created_at,
			 sync_id, sync_version, machine_id)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10)
		ON CONFLICT (sync_id) DO NOTHING`,
		e.FromNodeID, e.ToNodeID, e.Kind, e.SourceMsgID, e.BodyRevision, metadata, e.CreatedAt,
		e.SyncID, e.SyncVersion, e.MachineID,
	)
	return err
}

// ImportGraphArtifactIndex imports a graph.artifact_index row from sync.
func (db *DB) ImportGraphArtifactIndex(ctx context.Context, ai *SyncableGraphArtifactIndex) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.artifact_index
			(node_id, summary, summary_kind, refreshed_at,
			 sync_id, sync_version, machine_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (sync_id) DO NOTHING`,
		ai.NodeID, ai.Summary, ai.SummaryKind, ai.RefreshedAt,
		ai.SyncID, ai.SyncVersion, ai.MachineID,
	)
	return err
}

// ImportGraphArtifactBody imports a graph.artifact_bodies row from sync.
func (db *DB) ImportGraphArtifactBody(ctx context.Context, ab *SyncableGraphArtifactBody) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.artifact_bodies
			(node_id, body_full, ocr_text, description, fetched_at, expires_at,
			 sync_id, sync_version, machine_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (sync_id) DO NOTHING`,
		ab.NodeID, ab.BodyFull, ab.OCRText, ab.Description, ab.FetchedAt, ab.ExpiresAt,
		ab.SyncID, ab.SyncVersion, ab.MachineID,
	)
	return err
}

// ImportGraphSlackGroup imports a graph.slack_groups row from sync.
func (db *DB) ImportGraphSlackGroup(ctx context.Context, sg *SyncableGraphSlackGroup) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.slack_groups
			(id, handle, name, description, member_user_ids, user_count, refreshed_at,
			 sync_id, sync_version, machine_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (sync_id) DO NOTHING`,
		sg.ID, sg.Handle, sg.Name, sg.Description, sg.MemberUserIDs, sg.UserCount, sg.RefreshedAt,
		sg.SyncID, sg.SyncVersion, sg.MachineID,
	)
	return err
}

// ImportGraphEntity imports a graph.entities row from sync.
func (db *DB) ImportGraphEntity(ctx context.Context, e *SyncableGraphEntity) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.entities
			(id, kind, display_name, aliases, metadata, source, first_seen_at,
			 sync_id, sync_version, machine_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (sync_id) DO NOTHING`,
		e.ID, e.Kind, e.DisplayName, e.Aliases, e.Metadata, e.Source, e.FirstSeenAt,
		e.SyncID, e.SyncVersion, e.MachineID,
	)
	return err
}

// ImportGraphIdentityMap imports a graph.identity_map row from sync.
func (db *DB) ImportGraphIdentityMap(ctx context.Context, im *SyncableGraphIdentityMap) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.identity_map (source, external_id, person_id, resolved_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (source, external_id) DO NOTHING`,
		im.Source, im.ExternalID, im.PersonID, im.ResolvedAt,
	)
	return err
}

// ImportGraphJob imports a graph.jobs row from sync.
func (db *DB) ImportGraphJob(ctx context.Context, j *SyncableGraphJob) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.jobs
			(type, payload, priority, status, available_at, attempts, max_attempts,
			 last_error, locked_by, locked_at, enqueued_at, completed_at, target_runner,
			 sync_id, sync_version, machine_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (sync_id) DO NOTHING`,
		j.Type, j.Payload, j.Priority, j.Status, j.AvailableAt, j.Attempts, j.MaxAttempts,
		j.LastError, j.LockedBy, j.LockedAt, j.EnqueuedAt, j.CompletedAt, j.TargetRunner,
		j.SyncID, j.SyncVersion, j.MachineID,
	)
	return err
}

// ImportGraphUserAffinityConfig imports a graph.user_affinity_config row from sync.
func (db *DB) ImportGraphUserAffinityConfig(ctx context.Context, c *SyncableGraphUserAffinityConfig) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.user_affinity_config
			(eeid, team_group_ids, dept_group_ids, team_subtree_root_eeid,
			 autodetected, updated_at,
			 sync_id, sync_version, machine_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (sync_id) DO NOTHING`,
		c.EEID, c.TeamGroupIDs, c.DeptGroupIDs, c.TeamSubtreeRootEEID,
		c.Autodetected, c.UpdatedAt,
		c.SyncID, c.SyncVersion, c.MachineID,
	)
	return err
}

// ---------------------------------------------------------------------------
// GetGraphXxx ForPull — cursor-based pull (excludeSource = requesting machine)
// ---------------------------------------------------------------------------

// GetGraphPeopleForPull returns graph.people rows with id > afterID from other machines.
func (db *DB) GetGraphPeopleForPull(ctx context.Context, excludeSource string, afterID, limit int) ([]SyncableGraphPerson, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, eeid, email, display_name, slack_user_id, jira_account_id,
		       github_login, pagerduty_user_id, is_bot, reports_to, depth_from_root,
		       first_seen_at, identity_resolved_at, merged_into,
		       sync_id::text, sync_version, machine_id
		FROM graph.people
		WHERE machine_id IS DISTINCT FROM $1 AND id > $2
		ORDER BY id ASC LIMIT $3
	`, excludeSource, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("get graph people for pull: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphPerson
	for rows.Next() {
		var p SyncableGraphPerson
		if err := rows.Scan(
			&p.ID, &p.EEID, &p.Email, &p.DisplayName, &p.SlackUserID, &p.JiraAccountID,
			&p.GithubLogin, &p.PagerdutyUserID, &p.IsBot, &p.ReportsTo, &p.DepthFromRoot,
			&p.FirstSeenAt, &p.IdentityResolvedAt, &p.MergedInto,
			&p.SyncID, &p.SyncVersion, &p.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph person for pull: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetGraphNodesForPull returns graph.nodes rows updated after the cursor from other machines.
func (db *DB) GetGraphNodesForPull(ctx context.Context, excludeSource string, afterID, limit int) ([]SyncableGraphNode, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, type, natural_key, url, title, body, body_revision, body_ts,
		       mime_type, size_bytes, external_url, thumb_url,
		       author_person_id, scope, metadata, first_seen_at, updated_at, deleted_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.nodes
		WHERE machine_id IS DISTINCT FROM $1 AND ctid > (SELECT ctid FROM graph.nodes WHERE id = (
			SELECT id FROM graph.nodes ORDER BY updated_at ASC OFFSET $2 LIMIT 1
		))
		ORDER BY updated_at ASC LIMIT $3
	`, excludeSource, afterID, limit)
	if err != nil {
		// Fallback: simpler offset-based query
		rows2, err2 := db.Pool.Query(ctx, `
			SELECT id, type, natural_key, url, title, body, body_revision, body_ts,
			       mime_type, size_bytes, external_url, thumb_url,
			       author_person_id, scope, metadata, first_seen_at, updated_at, deleted_at,
			       sync_id::text, sync_version, machine_id
			FROM graph.nodes
			WHERE machine_id IS DISTINCT FROM $1
			ORDER BY updated_at ASC
			LIMIT $2 OFFSET $3
		`, excludeSource, limit, afterID)
		if err2 != nil {
			return nil, fmt.Errorf("get graph nodes for pull: %w", err2)
		}
		defer rows2.Close()
		var out []SyncableGraphNode
		for rows2.Next() {
			var n SyncableGraphNode
			if err := rows2.Scan(
				&n.ID, &n.Type, &n.NaturalKey, &n.URL, &n.Title, &n.Body, &n.BodyRevision, &n.BodyTS,
				&n.MimeType, &n.SizeBytes, &n.ExternalURL, &n.ThumbURL,
				&n.AuthorPersonID, &n.Scope, &n.Metadata, &n.FirstSeenAt, &n.UpdatedAt, &n.DeletedAt,
				&n.SyncID, &n.SyncVersion, &n.MachineID,
			); err != nil {
				return nil, fmt.Errorf("scan graph node for pull: %w", err)
			}
			out = append(out, n)
		}
		return out, rows2.Err()
	}
	defer rows.Close()

	var out []SyncableGraphNode
	for rows.Next() {
		var n SyncableGraphNode
		if err := rows.Scan(
			&n.ID, &n.Type, &n.NaturalKey, &n.URL, &n.Title, &n.Body, &n.BodyRevision, &n.BodyTS,
			&n.MimeType, &n.SizeBytes, &n.ExternalURL, &n.ThumbURL,
			&n.AuthorPersonID, &n.Scope, &n.Metadata, &n.FirstSeenAt, &n.UpdatedAt, &n.DeletedAt,
			&n.SyncID, &n.SyncVersion, &n.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph node for pull: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetGraphEdgesForPull returns graph.edges rows with id > afterID from other machines.
func (db *DB) GetGraphEdgesForPull(ctx context.Context, excludeSource string, afterID, limit int) ([]SyncableGraphEdge, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, from_node_id, to_node_id, kind, source_msg_id, body_revision, metadata, created_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.edges
		WHERE machine_id IS DISTINCT FROM $1 AND id > $2
		ORDER BY id ASC LIMIT $3
	`, excludeSource, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("get graph edges for pull: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphEdge
	for rows.Next() {
		var e SyncableGraphEdge
		if err := rows.Scan(
			&e.ID, &e.FromNodeID, &e.ToNodeID, &e.Kind, &e.SourceMsgID, &e.BodyRevision, &e.Metadata, &e.CreatedAt,
			&e.SyncID, &e.SyncVersion, &e.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph edge for pull: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetGraphArtifactIndexForPull returns graph.artifact_index rows from other machines.
// Embedding is excluded from sync transport; receiving machine re-generates via index_artifact job.
func (db *DB) GetGraphArtifactIndexForPull(ctx context.Context, excludeSource string, afterOffset, limit int) ([]SyncableGraphArtifactIndex, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT node_id, summary, summary_kind, refreshed_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.artifact_index
		WHERE machine_id IS DISTINCT FROM $1
		ORDER BY refreshed_at ASC
		LIMIT $2 OFFSET $3
	`, excludeSource, limit, afterOffset)
	if err != nil {
		return nil, fmt.Errorf("get graph artifact_index for pull: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphArtifactIndex
	for rows.Next() {
		var ai SyncableGraphArtifactIndex
		if err := rows.Scan(
			&ai.NodeID, &ai.Summary, &ai.SummaryKind, &ai.RefreshedAt,
			&ai.SyncID, &ai.SyncVersion, &ai.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph artifact_index for pull: %w", err)
		}
		out = append(out, ai)
	}
	return out, rows.Err()
}

// GetGraphArtifactBodiesForPull returns graph.artifact_bodies rows from other machines.
func (db *DB) GetGraphArtifactBodiesForPull(ctx context.Context, excludeSource string, afterOffset, limit int) ([]SyncableGraphArtifactBody, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT node_id, body_full, ocr_text, description, fetched_at, expires_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.artifact_bodies
		WHERE machine_id IS DISTINCT FROM $1
		ORDER BY fetched_at ASC
		LIMIT $2 OFFSET $3
	`, excludeSource, limit, afterOffset)
	if err != nil {
		return nil, fmt.Errorf("get graph artifact_bodies for pull: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphArtifactBody
	for rows.Next() {
		var ab SyncableGraphArtifactBody
		if err := rows.Scan(
			&ab.NodeID, &ab.BodyFull, &ab.OCRText, &ab.Description, &ab.FetchedAt, &ab.ExpiresAt,
			&ab.SyncID, &ab.SyncVersion, &ab.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph artifact_body for pull: %w", err)
		}
		out = append(out, ab)
	}
	return out, rows.Err()
}

// GetGraphSlackGroupsForPull returns graph.slack_groups rows from other machines.
func (db *DB) GetGraphSlackGroupsForPull(ctx context.Context, excludeSource string, afterOffset, limit int) ([]SyncableGraphSlackGroup, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, handle, name, description, member_user_ids, user_count, refreshed_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.slack_groups
		WHERE machine_id IS DISTINCT FROM $1
		ORDER BY id ASC
		LIMIT $2 OFFSET $3
	`, excludeSource, limit, afterOffset)
	if err != nil {
		return nil, fmt.Errorf("get graph slack_groups for pull: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphSlackGroup
	for rows.Next() {
		var sg SyncableGraphSlackGroup
		if err := rows.Scan(
			&sg.ID, &sg.Handle, &sg.Name, &sg.Description, &sg.MemberUserIDs, &sg.UserCount, &sg.RefreshedAt,
			&sg.SyncID, &sg.SyncVersion, &sg.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph slack_group for pull: %w", err)
		}
		out = append(out, sg)
	}
	return out, rows.Err()
}

// GetGraphEntitiesForPull returns graph.entities rows from other machines.
func (db *DB) GetGraphEntitiesForPull(ctx context.Context, excludeSource string, afterOffset, limit int) ([]SyncableGraphEntity, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, kind, display_name, aliases, metadata, source, first_seen_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.entities
		WHERE machine_id IS DISTINCT FROM $1
		ORDER BY id ASC
		LIMIT $2 OFFSET $3
	`, excludeSource, limit, afterOffset)
	if err != nil {
		return nil, fmt.Errorf("get graph entities for pull: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphEntity
	for rows.Next() {
		var e SyncableGraphEntity
		if err := rows.Scan(
			&e.ID, &e.Kind, &e.DisplayName, &e.Aliases, &e.Metadata, &e.Source, &e.FirstSeenAt,
			&e.SyncID, &e.SyncVersion, &e.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph entity for pull: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetGraphJobsForPull returns graph.jobs rows from other machines (target_runner != 'local').
func (db *DB) GetGraphJobsForPull(ctx context.Context, excludeSource string, afterID, limit int) ([]SyncableGraphJob, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, type, payload, priority, status, available_at, attempts, max_attempts,
		       last_error, locked_by, locked_at, enqueued_at, completed_at, target_runner,
		       sync_id::text, sync_version, machine_id
		FROM graph.jobs
		WHERE machine_id IS DISTINCT FROM $1 AND id > $2 AND target_runner != 'local'
		ORDER BY id ASC LIMIT $3
	`, excludeSource, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("get graph jobs for pull: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphJob
	for rows.Next() {
		var j SyncableGraphJob
		if err := rows.Scan(
			&j.ID, &j.Type, &j.Payload, &j.Priority, &j.Status, &j.AvailableAt, &j.Attempts, &j.MaxAttempts,
			&j.LastError, &j.LockedBy, &j.LockedAt, &j.EnqueuedAt, &j.CompletedAt, &j.TargetRunner,
			&j.SyncID, &j.SyncVersion, &j.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph job for pull: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// GetGraphUserAffinityConfigForPull returns graph.user_affinity_config rows from other machines.
func (db *DB) GetGraphUserAffinityConfigForPull(ctx context.Context, excludeSource string, afterOffset, limit int) ([]SyncableGraphUserAffinityConfig, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT eeid, team_group_ids, dept_group_ids, team_subtree_root_eeid,
		       autodetected, updated_at,
		       sync_id::text, sync_version, machine_id
		FROM graph.user_affinity_config
		WHERE machine_id IS DISTINCT FROM $1
		ORDER BY eeid ASC
		LIMIT $2 OFFSET $3
	`, excludeSource, limit, afterOffset)
	if err != nil {
		return nil, fmt.Errorf("get graph user_affinity_config for pull: %w", err)
	}
	defer rows.Close()

	var out []SyncableGraphUserAffinityConfig
	for rows.Next() {
		var c SyncableGraphUserAffinityConfig
		if err := rows.Scan(
			&c.EEID, &c.TeamGroupIDs, &c.DeptGroupIDs, &c.TeamSubtreeRootEEID,
			&c.Autodetected, &c.UpdatedAt,
			&c.SyncID, &c.SyncVersion, &c.MachineID,
		); err != nil {
			return nil, fmt.Errorf("scan graph user_affinity_config for pull: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
