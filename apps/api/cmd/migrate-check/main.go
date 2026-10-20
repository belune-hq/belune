// Command migrate-check applies all embedded migrations against the database
// pointed to by DATABASE_URL and exits. Used by CI to catch migration drift
// (a SQL file that fails to apply on a fresh DB) before tagging a release.
//
// Exits 0 on success, 1 on any error. Logs JSON to stdout.
package main

import (
	"log/slog"
	"os"

	"github.com/belune-hq/belune/internal/migrations"
	"github.com/belune-hq/belune/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	if err := store.RunMigrations(databaseURL, migrations.Files); err != nil {
		slog.Error("migrations failed", "error", err)
		os.Exit(1)
	}
	slog.Info("migrations applied successfully")
}
