package service

import (
	"context"
	"log/slog"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/store/generated"
)

// BackfillWebhookSecrets moves any plaintext webhook secrets into the encrypted
// column. Migration 000051 could not do this itself — encryption needs the
// keyring, which SQL cannot reach.
//
// Deliberately best-effort and non-fatal: a row that fails to encrypt keeps its
// plaintext secret, and every read path falls back to that column, so a partial
// backfill degrades to the old behaviour rather than breaking push deploys. It
// is also idempotent, since the query only returns rows still holding
// plaintext.
func BackfillWebhookSecrets(ctx context.Context, queries *generated.Queries, keyring *crypto.Keyring) {
	if keyring == nil {
		return
	}

	rows, err := queries.ListApplicationsWithPlaintextWebhookSecret(ctx)
	if err != nil {
		slog.Warn("webhook secret backfill: could not list applications", "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}

	migrated := 0
	for _, app := range rows {
		encrypted, encErr := keyring.Encrypt([]byte(app.WebhookSecret.String))
		if encErr != nil {
			slog.Error("webhook secret backfill: encrypt failed", "application", app.Name, "error", encErr)
			continue
		}
		if err := queries.BackfillWebhookSecret(ctx, generated.BackfillWebhookSecretParams{
			ID:                     app.ID,
			WebhookSecretEncrypted: encrypted,
		}); err != nil {
			slog.Error("webhook secret backfill: write failed", "application", app.Name, "error", err)
			continue
		}
		migrated++
	}

	slog.Info("webhook secret backfill complete", "migrated", migrated, "total", len(rows))
}
