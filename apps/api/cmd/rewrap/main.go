// Command rewrap re-encrypts every secret column under the current KEK.
//
// Safe to run repeatedly: rows already tagged with the current KEK version are
// skipped. Rows encrypted with an older KEK (or the pre-keyring legacy format)
// are decrypted and re-sealed with the current KEK's envelope, without altering
// the plaintext.
//
// Usage:
//
//	ENCRYPTION_KEYS=v1:... ,v2:... ENCRYPTION_KEY_CURRENT=v2 \
//	    DATABASE_URL=postgres://... go run ./cmd/rewrap [--dry-run]
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/store"
)

// target describes one (table, id column, encrypted column) tuple to rewrap.
type target struct {
	table  string
	idCol  string
	colEnc string
}

// Every *_encrypted column in the schema belongs here. A column that is missing
// is not skipped loudly — rewrap reports success, the operator retires the old
// KEK, and the secret becomes undecryptable. TestTargetsCoverEveryEncryptedColumn
// fails when the schema and this list disagree, so add the column here in the
// same change that creates it.
var targets = []target{
	// git_credentials is deliberately absent: migration 000020 folded it into
	// applications.git_credentials_encrypted and dropped the table, so the
	// target that used to be here made every run fail its select and exit 1.
	{"applications", "id", "git_credentials_encrypted"},
	{"applications", "id", "webhook_secret_encrypted"},
	{"applications", "id", "deploy_hook_token_encrypted"},
	{"application_file_mounts", "id", "content_encrypted"},
	{"databases", "id", "credentials_encrypted"},
	{"domains", "id", "ssl_credentials_encrypted"},
	{"certificates", "id", "cert_pem_encrypted"},
	{"certificates", "id", "key_pem_encrypted"},
	{"env_vars", "id", "value_encrypted"},
	{"project_env_vars", "id", "value_encrypted"},
	{"git_provider_configs", "id", "secret_encrypted"},
	{"git_integrations", "id", "config_encrypted"},
	{"backup_destinations", "id", "credentials_encrypted"},
	{"notification_channels", "id", "config_encrypted"},
	{"dns_providers", "id", "credentials_encrypted"},
	// The id is the text 'default' (a singleton), which rewrapTable scans as a
	// string like any other key.
	{"crowdsec_config", "id", "bouncer_key_encrypted"},
	{"users", "id", "totp_secret_encrypted"},
	// A tombstone holds the deleted database's credentials so a replacement
	// restore can bring applications back without an env-var edit. Same secret,
	// same rotation requirement.
	{"database_tombstones", "id", "credentials_encrypted"},
}

// settingTargets are secrets that live in a settings ROW rather than a column,
// base64-encoded because the value column is text. They are invisible to the
// schema — nothing about `settings` says one row holds ciphertext — which is
// exactly why they need naming here, and why the coverage test checks the rows
// present in the database as well as the columns.
var settingTargets = []string{
	"smtp_password_encrypted",
}

func main() {
	dryRun := flag.Bool("dry-run", false, "report what would be rewrapped without writing")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	pool, err := store.Connect(cfg.DatabaseURL)
	if err != nil {
		slog.Error("connect database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	ctx := context.Background()
	slog.Info("rewrap starting",
		"current_kek", cfg.Keyring.CurrentVersion(),
		"available_keks", cfg.Keyring.Versions(),
		"dry_run", *dryRun,
	)

	var total, rewrapped, skipped, failed int
	for _, key := range settingTargets {
		r, err := rewrapSetting(ctx, pool, cfg.Keyring, key, *dryRun)
		if err != nil {
			slog.Error("setting failed", "key", key, "error", err)
			failed++
			continue
		}
		total += r.scanned
		rewrapped += r.rewrapped
		skipped += r.skipped
		slog.Info("setting done",
			"key", key,
			"scanned", r.scanned,
			"rewrapped", r.rewrapped,
			"skipped_current", r.skipped,
		)
	}
	for _, t := range targets {
		r, err := rewrapTable(ctx, pool, cfg.Keyring, t, *dryRun)
		if err != nil {
			slog.Error("table failed", "table", t.table, "error", err)
			failed++
			continue
		}
		total += r.scanned
		rewrapped += r.rewrapped
		skipped += r.skipped
		slog.Info("table done",
			"table", t.table,
			"scanned", r.scanned,
			"rewrapped", r.rewrapped,
			"skipped_current", r.skipped,
		)
	}

	slog.Info("rewrap complete",
		"total_scanned", total,
		"rewrapped", rewrapped,
		"skipped_current", skipped,
		"failed_tables", failed,
	)
	if failed > 0 {
		os.Exit(1)
	}
}

type result struct {
	scanned, rewrapped, skipped int
}

// rewrapSetting re-seals a base64-encoded secret stored in a settings row. A
// missing row is not an error: the setting simply has never been configured.
func rewrapSetting(ctx context.Context, pool *pgxpool.Pool, kr *crypto.Keyring, key string, dryRun bool) (result, error) {
	var res result

	var encoded string
	err := pool.QueryRow(ctx, "SELECT value FROM settings WHERE key = $1", key).Scan(&encoded)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return res, nil
		}
		return res, fmt.Errorf("select: %w", err)
	}
	if encoded == "" {
		return res, nil
	}
	res.scanned++

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return res, fmt.Errorf("decode: %w", err)
	}
	if kr.IsCurrent(raw) {
		res.skipped++
		return res, nil
	}

	upgraded, err := kr.Rewrap(raw)
	if err != nil {
		return res, fmt.Errorf("rewrap: %w", err)
	}
	res.rewrapped++
	if dryRun {
		return res, nil
	}
	if _, err := pool.Exec(ctx,
		"UPDATE settings SET value = $2 WHERE key = $1",
		key, base64.StdEncoding.EncodeToString(upgraded),
	); err != nil {
		return res, fmt.Errorf("update: %w", err)
	}
	return res, nil
}

func rewrapTable(ctx context.Context, pool *pgxpool.Pool, kr *crypto.Keyring, t target, dryRun bool) (result, error) {
	var res result

	selectSQL := fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s IS NOT NULL", t.idCol, t.colEnc, t.table, t.colEnc)
	rows, err := pool.Query(ctx, selectSQL)
	if err != nil {
		return res, fmt.Errorf("select: %w", err)
	}

	type row struct {
		id  string
		enc []byte
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.enc); err != nil {
			rows.Close()
			return res, fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("iterate: %w", err)
	}

	updateSQL := fmt.Sprintf("UPDATE %s SET %s = $1 WHERE %s = $2", t.table, t.colEnc, t.idCol)
	for _, r := range batch {
		res.scanned++
		if len(r.enc) == 0 {
			res.skipped++
			continue
		}
		if kr.IsCurrent(r.enc) {
			res.skipped++
			continue
		}
		upgraded, err := kr.Rewrap(r.enc)
		if err != nil {
			return res, fmt.Errorf("rewrap row %s: %w", r.id, err)
		}
		if dryRun {
			res.rewrapped++
			continue
		}
		if _, err := pool.Exec(ctx, updateSQL, upgraded, r.id); err != nil {
			return res, fmt.Errorf("update row %s: %w", r.id, err)
		}
		res.rewrapped++
	}
	return res, nil
}
