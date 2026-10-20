package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

// TestTargetsCoverEveryEncryptedColumn asks the schema, not a list someone has
// to remember to update. Forgetting a column here is silent in the worst way:
// rewrap reports success, the operator retires the old KEK believing every
// secret has been re-sealed, and the ones it skipped can no longer be
// decrypted. The failure surfaces months later, on a key rotation, with no
// path back.
//
// This is the same shape as internal/config's documented_test: the fix for
// "someone will forget" is something that fails when the two disagree.
func TestTargetsCoverEveryEncryptedColumn(t *testing.T) {
	pool, _, teardown := testutil.SetupTestDB()
	defer teardown()

	rows, err := pool.Query(context.Background(), `
		SELECT table_name, column_name
		FROM   information_schema.columns
		WHERE  table_schema = 'public' AND column_name LIKE '%\_encrypted'
		ORDER  BY table_name, column_name
	`)
	require.NoError(t, err)
	defer rows.Close()

	covered := make(map[string]bool, len(targets))
	for _, tgt := range targets {
		covered[tgt.table+"."+tgt.colEnc] = true
	}

	var found []string
	for rows.Next() {
		var table, column string
		require.NoError(t, rows.Scan(&table, &column))
		found = append(found, table+"."+column)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, found, "no *_encrypted columns found — the query or the schema is wrong")

	for _, col := range found {
		assert.True(t, covered[col],
			"%s is encrypted but rewrap does not rewrap it: key rotation would skip it and "+
				"retiring the old KEK would make it undecryptable. Add it to targets.", col)
	}

	// The converse: a target naming a column that no longer exists would fail at
	// runtime, mid-rotation, on a half-rewrapped database.
	inSchema := make(map[string]bool, len(found))
	for _, col := range found {
		inSchema[col] = true
	}
	for _, tgt := range targets {
		name := tgt.table + "." + tgt.colEnc
		assert.True(t, inSchema[name], "rewrap targets %s, which is not in the schema", name)
	}

	fmt.Printf("rewrap covers %d encrypted columns\n", len(found))
}

// TestSettingTargetsCoverEncryptedSettings closes the hole the column test
// cannot see. A secret stored in a settings ROW (the SMTP password) is
// invisible to information_schema — nothing about the settings table says one
// row holds ciphertext — so the coverage claim was false for it while every
// column-level check passed.
//
// ⚠️ This can only check keys that exist in the database it runs against, so it
// seeds the ones rewrap knows about and asserts every encrypted-looking key
// present is covered. A brand-new settings secret that nobody registers here
// will still slip through: the durable fix is to stop putting secrets in
// settings rows.
func TestSettingTargetsCoverEncryptedSettings(t *testing.T) {
	pool, _, teardown := testutil.SetupTestDB()
	defer teardown()
	ctx := context.Background()

	// Seed each known target so the query below has something to find, proving
	// the check is not vacuous.
	for _, key := range settingTargets {
		_, err := pool.Exec(ctx,
			`INSERT INTO settings (key, value) VALUES ($1, 'seeded')
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key)
		require.NoError(t, err)
	}

	rows, err := pool.Query(ctx,
		`SELECT key FROM settings WHERE key LIKE '%\_encrypted' ORDER BY key`)
	require.NoError(t, err)
	defer rows.Close()

	covered := make(map[string]bool, len(settingTargets))
	for _, key := range settingTargets {
		covered[key] = true
	}

	var found []string
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		found = append(found, key)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, found, "the seeded settings should have been found")

	for _, key := range found {
		assert.True(t, covered[key],
			"settings key %s holds an encrypted value that rewrap does not re-seal: "+
				"key rotation would skip it and retiring the old KEK would make it "+
				"undecryptable. Add it to settingTargets.", key)
	}
}
