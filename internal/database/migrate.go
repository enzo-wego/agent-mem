package database

import (
	"database/sql"
	"errors"
	"fmt"
	stdlog "log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	zlog "github.com/rs/zerolog/log"
)

const (
	defaultMigrationsDir = "./migrations"
	dockerMigrationsDir  = "/usr/local/share/agent-mem/migrations"
	minVersion           = int64(0)
	maxVersion           = int64((1 << 63) - 1)
)

func init() {
	goose.SetLogger(goose.NopLogger())
}

// migrationsDir returns the path to the migrations directory.
// In Docker, migrations are bundled at /usr/local/share/agent-mem/migrations.
func migrationsDir() string {
	if _, err := os.Stat(dockerMigrationsDir); err == nil {
		return dockerMigrationsDir
	}
	return defaultMigrationsDir
}

func openStdlib(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return db, nil
}

// RunMigrations applies all pending goose migrations.
func RunMigrations(databaseURL string) error {
	db, err := openStdlib(databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	dir := migrationsDir()
	zlog.Info().Str("dir", dir).Msg("Running migrations")

	if err := goose.Up(db, dir, goose.WithAllowMissing()); err != nil {
		if errors.Is(err, goose.ErrNoNextVersion) {
			zlog.Info().Msg("No new migrations to apply")
			return nil
		}
		return fmt.Errorf("run migrations: %w", err)
	}

	zlog.Info().Msg("Migrations applied")
	return nil
}

// MigrateCreate creates a new migration file.
func MigrateCreate(name string) error {
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	return goose.Create(nil, defaultMigrationsDir, name, "sql")
}

// MigrateStatus prints the status of all migrations.
func MigrateStatus(databaseURL string) error {
	db, err := openStdlib(databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	// Enable goose logging for status output
	goose.SetLogger(stdlog.Default())
	defer goose.SetLogger(goose.NopLogger())

	return goose.Status(db, migrationsDir())
}

// MigrateRollback rolls back the last migration, or to a specific version if > 0.
func MigrateRollback(databaseURL string, version int64) error {
	db, err := openStdlib(databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	dir := migrationsDir()

	if version == 0 {
		if err := goose.Down(db, dir); err != nil {
			return fmt.Errorf("rollback: %w", err)
		}
	} else {
		migrations, err := goose.CollectMigrations(dir, minVersion, maxVersion)
		if err != nil {
			return fmt.Errorf("collect migrations: %w", err)
		}

		target, err := migrations.Current(version)
		if target == nil && err != nil {
			return fmt.Errorf("version %d not found: %w", version, err)
		}

		if err := goose.DownTo(db, dir, version); err != nil {
			return fmt.Errorf("rollback to version %d: %w", version, err)
		}
	}

	zlog.Info().Msg("Migration rolled back")
	_ = goose.Version(db, dir)
	return nil
}

// MigrateUpByOne applies the next pending migration.
func MigrateUpByOne(databaseURL string) error {
	db, err := openStdlib(databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	dir := migrationsDir()

	if err := goose.UpByOne(db, dir, goose.WithAllowMissing()); err != nil {
		if errors.Is(err, goose.ErrNoNextVersion) {
			zlog.Info().Msg("No pending migrations to apply")
			return nil
		}
		return fmt.Errorf("migrate up by one: %w", err)
	}

	zlog.Info().Msg("Applied one migration")
	_ = goose.Version(db, dir)
	return nil
}

// MigrateFix force-deletes a migration version record from goose_db_version.
// Use this to remove a failed/stuck migration so it can be re-run.
func MigrateFix(databaseURL string, version int64) error {
	db, err := openStdlib(databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	result, err := db.Exec("DELETE FROM goose_db_version WHERE version_id = $1", version)
	if err != nil {
		return fmt.Errorf("delete version %d: %w", version, err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("version %d not found in goose_db_version", version)
	}

	zlog.Info().Int64("version", version).Msg("Deleted migration record from goose_db_version")
	return nil
}
