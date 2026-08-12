// Package db owns the two real backing connections (Postgres, Redis) that
// replace the InMemory* stand-ins across limits/conversation/moderation/
// idempotency/costlog -- see README's "Current task: local dev
// infrastructure" section. Nothing here knows about any of those
// packages' interfaces; cmd/server/main.go wires the concrete stores on
// top of the *sql.DB / *redis.Client this package returns.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

// Connect opens a Postgres connection pool from POSTGRES_* env vars
// (POSTGRES_USER/POSTGRES_PASSWORD/POSTGRES_DB required; POSTGRES_HOST/
// POSTGRES_PORT default to the docker-compose published address), pings
// it, and applies every pending embedded migration (see migrate.go)
// before returning -- callers never need a separate migration step.
func Connect(ctx context.Context) (*sql.DB, error) {
	user, err := requireGetenv("POSTGRES_USER")
	if err != nil {
		return nil, err
	}
	password, err := requireGetenv("POSTGRES_PASSWORD")
	if err != nil {
		return nil, err
	}
	dbname, err := requireGetenv("POSTGRES_DB")
	if err != nil {
		return nil, err
	}
	host := getenvDefault("POSTGRES_HOST", "127.0.0.1")
	port := getenvDefault("POSTGRES_PORT", "5432")

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, password, host, port, dbname)

	pgDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open postgres: %w", err)
	}
	// Kept comfortably under docker-compose.yml's postgres max_connections=20
	// -- this is the only process talking to it on a dev box.
	pgDB.SetMaxOpenConns(10)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pgDB.PingContext(pingCtx); err != nil {
		pgDB.Close()
		return nil, fmt.Errorf("db: ping postgres: %w", err)
	}

	if err := Migrate(ctx, pgDB); err != nil {
		pgDB.Close()
		return nil, err
	}

	return pgDB, nil
}

// ConnectRedis opens a Redis client from REDIS_* env vars
// (REDIS_PASSWORD required; REDIS_HOST/REDIS_PORT default to the
// docker-compose published address) and confirms it's reachable with a
// PING before returning.
func ConnectRedis(ctx context.Context) (*redis.Client, error) {
	password, err := requireGetenv("REDIS_PASSWORD")
	if err != nil {
		return nil, err
	}
	host := getenvDefault("REDIS_HOST", "127.0.0.1")
	port := getenvDefault("REDIS_PORT", "6379")

	rdb := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", host, port),
		Password: password,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("db: ping redis: %w", err)
	}

	return rdb, nil
}

func requireGetenv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("db: %s is required (see .env.example)", key)
	}
	return v, nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
