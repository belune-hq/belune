package store_test

import (
	"bufio"
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/waf"
)

// The whole recording path against a real database: real lines from Caddy, the
// recorder's hook and batch writer, and a real domains row for the host to resolve
// to. The unit tests cover each piece against a fake store; this is the seam
// between them and Postgres — the inet column, the COPY, the foreign key.
func TestWAFPipeline_RealCaddyLinesBecomeRows(t *testing.T) {
	ctx := resetWAF(t)

	user, err := testQueries.CreateUser(ctx, generated.CreateUserParams{Email: "pipe@test.com", PasswordHash: "h", Role: "admin"})
	require.NoError(t, err)
	proj, err := testQueries.CreateProject(ctx, generated.CreateProjectParams{
		Name: "p", Slug: "p", UserID: user.ID, ServerID: testutil.LocalServerID(t, ctx, testQueries),
	})
	require.NoError(t, err)
	app, err := testQueries.CreateApplication(ctx, generated.CreateApplicationParams{
		ProjectID: proj.ID, Name: "a", Slug: "a", Type: "image", BuildType: "image",
	})
	require.NoError(t, err)
	// The captured lines were produced by requests to this host.
	_, err = testQueries.CreateDomain(ctx, generated.CreateDomainParams{
		ApplicationID: app.ID, Hostname: "app.test", SslMode: "off", Path: "/",
	})
	require.NoError(t, err)

	rec := waf.NewRecorder(testQueries)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { rec.Run(runCtx); close(done) }()

	f, err := os.Open("../waf/testdata/caddy-coraza-crs-4.25.0.jsonl")
	require.NoError(t, err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		rec.HandleCaddyLine(ctx, sc.Text())
	}
	cancel() // shutdown flushes what is queued
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the recorder did not shut down")
	}

	rows, err := testQueries.ListWAFEvents(ctx, generated.ListWAFEventsParams{RowLimit: 50})
	require.NoError(t, err)
	require.Len(t, rows, 6, "six rule matches in the capture; the engine's debug lines and an unrelated line are not events")
	assert.EqualValues(t, 6, rec.Stats().Recorded)
	assert.Zero(t, rec.Stats().Dropped+rec.Stats().Malformed)

	byRule := map[int32]generated.ListWAFEventsRow{}
	for _, e := range rows {
		byRule[e.RuleID] = e
		assert.Equal(t, "127.0.0.1", e.ClientIp.String())
		assert.Equal(t, "app.test", e.Host.String, "the host rode in the transaction id")
		assert.Equal(t, app.ID, e.ApplicationID, "and resolved to the application serving it")
		assert.True(t, e.TxID.Valid)
	}
	assert.Equal(t, "attack-xss", byRule[941100].Category.String)
	assert.Equal(t, "XSS Attack Detected via libinjection", byRule[941100].Message)
	assert.Equal(t, "unknown", byRule[949110].Severity, "the anomaly summary declares no severity")
	assert.Equal(t, "Inbound Anomaly Score Exceeded", byRule[949110].Message)
	// The capture ran DetectionOnly, so every line opens "Warning.": read from
	// real Coraza output, not from a string the parser's author wrote.
	for _, e := range byRule {
		assert.Equal(t, "detected", e.Disposition.String, "rule %d", e.RuleID)
		assert.False(t, e.RequestBlocked)
	}
	assert.Equal(t, int32(25), byRule[949110].AnomalyScore.Int32, "recovered from the message staticMessage cuts")
	assert.False(t, byRule[941100].AnomalyScore.Valid, "only the scoring rule has one")

	// Nothing the request carried was kept, in any column of any row.
	var leaks int
	require.NoError(t, testPool.QueryRow(ctx, `
		SELECT count(*) FROM waf_events w
		WHERE to_jsonb(w)::text ~* '(<script|alert\(|%3C|/s\?q|matched data|1=1)'`).Scan(&leaks))
	assert.Zero(t, leaks, "the table holds no payload and no URI")
}
