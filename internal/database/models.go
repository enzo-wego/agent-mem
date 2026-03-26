package database

import (
	"time"

	"github.com/pgvector/pgvector-go"
)

type SdkSession struct {
	ID               int        `db:"id"`
	ContentSessionID string     `db:"content_session_id"`
	MemorySessionID  *string    `db:"memory_session_id"`
	Project          string     `db:"project"`
	UserPrompt       *string    `db:"user_prompt"`
	StartedAt        time.Time  `db:"started_at"`
	StartedAtEpoch   int64      `db:"started_at_epoch"`
	CompletedAt      *time.Time `db:"completed_at"`
	CompletedAtEpoch *int64     `db:"completed_at_epoch"`
	Status           string     `db:"status"`
	SyncID           *string    `db:"sync_id"`
	SyncVersion      int        `db:"sync_version"`
	SyncSource       *string    `db:"sync_source"`
}

type Observation struct {
	ID              int             `db:"id"`
	MemorySessionID string          `db:"memory_session_id"`
	Project         string          `db:"project"`
	Type            string          `db:"type"`
	Title           *string         `db:"title"`
	Subtitle        *string         `db:"subtitle"`
	Narrative       *string         `db:"narrative"`
	Text            *string         `db:"text"`
	Facts           []byte          `db:"facts"`
	Concepts        []byte          `db:"concepts"`
	FilesRead       []byte          `db:"files_read"`
	FilesModified   []byte          `db:"files_modified"`
	DiscoveryTokens int             `db:"discovery_tokens"`
	CreatedAt       time.Time       `db:"created_at"`
	CreatedAtEpoch  int64           `db:"created_at_epoch"`
	Embedding       *pgvector.Vector `db:"embedding"`
	SyncID          *string         `db:"sync_id"`
	SyncVersion     int             `db:"sync_version"`
	SyncSource      *string         `db:"sync_source"`
}

type SessionSummary struct {
	ID              int             `db:"id"`
	MemorySessionID string          `db:"memory_session_id"`
	Project         string          `db:"project"`
	Request         *string         `db:"request"`
	Investigated    *string         `db:"investigated"`
	Learned         *string         `db:"learned"`
	Completed       *string         `db:"completed"`
	NextSteps       *string         `db:"next_steps"`
	Notes           *string         `db:"notes"`
	FilesRead       []byte          `db:"files_read"`
	FilesEdited     []byte          `db:"files_edited"`
	DiscoveryTokens int             `db:"discovery_tokens"`
	CreatedAt       time.Time       `db:"created_at"`
	CreatedAtEpoch  int64           `db:"created_at_epoch"`
	Embedding       *pgvector.Vector `db:"embedding"`
	SyncID          *string         `db:"sync_id"`
	SyncVersion     int             `db:"sync_version"`
	SyncSource      *string         `db:"sync_source"`
}

type UserPrompt struct {
	ID               int             `db:"id"`
	ContentSessionID string          `db:"content_session_id"`
	Project          string          `db:"project"`
	Prompt           string          `db:"prompt"`
	PromptNumber     int             `db:"prompt_number"`
	CreatedAt        time.Time       `db:"created_at"`
	CreatedAtEpoch   int64           `db:"created_at_epoch"`
	Embedding        *pgvector.Vector `db:"embedding"`
	SyncID           *string         `db:"sync_id"`
	SyncVersion      int             `db:"sync_version"`
	SyncSource       *string         `db:"sync_source"`
}

type PendingMessage struct {
	ID               int        `db:"id"`
	ContentSessionID string     `db:"content_session_id"`
	MessageType      string     `db:"message_type"`
	Payload          []byte     `db:"payload"`
	Status           string     `db:"status"`
	CreatedAt        time.Time  `db:"created_at"`
	ProcessedAt      *time.Time `db:"processed_at"`
	Error            *string    `db:"error"`
}

type SchemaVersion struct {
	Version     int       `db:"version"`
	AppliedAt   time.Time `db:"applied_at"`
	Description *string   `db:"description"`
}
