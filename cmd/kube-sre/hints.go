package main

import (
	"fmt"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

// dbHint says how to fix a common database error.
func dbHint(err error, cfg *config.Config) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "password authentication failed"):
		return fmt.Sprintf("The password does not match the postgres user %q.\n  Check POSTGRES_PASSWORD in ~/.kube-sre/.env, then re-run: kube-sre db-init", cfg.PGUser)
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "connection failed") || strings.Contains(msg, "no such host"):
		return fmt.Sprintf("Cannot connect to postgres at %s:%s. Is it running?\n  Start one with Docker:\n"+
			"    docker run -d --name ks-pg -e POSTGRES_USER=%s -e POSTGRES_PASSWORD=<password> -e POSTGRES_DB=%s -p %s:5432 postgres:16\n"+
			"  Or use SQLite by unsetting POSTGRES_HOST and DATABASE_URL, or set USE_SQLITE=true.",
			cfg.PGHost, cfg.PGPort, cfg.PGUser, cfg.PGDatabase, cfg.PGPort)
	case strings.Contains(msg, "does not exist") && strings.Contains(msg, "database"):
		return fmt.Sprintf("Database %q does not exist.\n  Create it first: createdb -h %s -U %s %s\n  Then re-run: kube-sre db-init",
			cfg.PGDatabase, cfg.PGHost, cfg.PGUser, cfg.PGDatabase)
	case strings.Contains(msg, "role") && strings.Contains(msg, "does not exist"):
		return fmt.Sprintf("Postgres user %q does not exist.\n  Check POSTGRES_USER, or create the role: createuser -h %s -s %s", cfg.PGUser, cfg.PGHost, cfg.PGUser)
	case strings.Contains(msg, "ssl"):
		return "SSL/TLS connection error.\n  If your postgres requires SSL, add ?sslmode=require to DATABASE_URL."
	}
	return "Check your database configuration in ~/.kube-sre/.env."
}
