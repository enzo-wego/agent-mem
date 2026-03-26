package database

import (
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps a connection pool and provides query methods.
type DB struct {
	Pool *pgxpool.Pool
}

func NewDB(pool *pgxpool.Pool) *DB {
	return &DB{Pool: pool}
}
