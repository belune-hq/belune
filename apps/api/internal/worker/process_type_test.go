package worker

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/store/generated"
)

// The backfilled web type: command ”, routable, both health columns NULL. Every
// existing install has exactly this and nothing else, so B1 is invisible only if it
// resolves to precisely what the application alone gave.
func backfilledWeb() generated.ApplicationProcessType {
	return generated.ApplicationProcessType{Name: "web", Command: "", Replicas: 1, Routable: true}
}

func TestBackfilledWebType_ChangesNothing(t *testing.T) {
	for _, app := range []generated.Application{
		{HealthCheckType: "http", HealthCheckPath: txt("/up")},
		{HealthCheckType: "none"},
		{HealthCheckType: "command", HealthCheckCommand: txt("pg_isready"), HealthCheckRetries: i4(5)},
	} {
		got := resolveHealthCheck(app, backfilledWeb())
		assert.Equal(t, app.HealthCheckType, got.typ, "a backfilled type inherits the application's check type")
		assert.Equal(t, app.HealthCheckCommand, got.command)
		// ...and the Docker HEALTHCHECK is identical to the one the app alone produced.
		assert.Equal(t, healthCheckRuntimeConfig(app, inherited(app)), healthCheckRuntimeConfig(app, got))
	}

	// An empty command is "leave the image's CMD alone": nil, never []string{""}.
	assert.Nil(t, commandArgv(backfilledWeb().Command))
	assert.Nil(t, commandArgv("   \t"))
}

func TestResolveHealthCheck_TheThreeBranches(t *testing.T) {
	app := generated.Application{HealthCheckType: "http", HealthCheckPath: txt("/up"), HealthCheckCommand: txt("app-wide")}

	// 1. The type sets one: use it (and its command, else the application's).
	pt := generated.ApplicationProcessType{Routable: false, HealthCheckType: txt("command"), HealthCheckCommand: txt("pgrep worker")}
	got := resolveHealthCheck(app, pt)
	assert.Equal(t, "command", got.typ)
	assert.Equal(t, "pgrep worker", got.command.String)

	pt.HealthCheckCommand = pgtype.Text{}
	got = resolveHealthCheck(app, pt)
	assert.Equal(t, "command", got.typ)
	assert.Equal(t, "app-wide", got.command.String, "a NULL command on the type inherits the application's")

	// 2. Routable, no type of its own: inherit the application's.
	got = resolveHealthCheck(app, generated.ApplicationProcessType{Routable: true})
	assert.Equal(t, "http", got.typ)

	// 3. ⚠️ Not routable, no type of its own: 'none', NEVER 'http'. A worker listens
	// on nothing, so inheriting http would leave every worker permanently unhealthy.
	got = resolveHealthCheck(app, generated.ApplicationProcessType{Routable: false})
	assert.Equal(t, "none", got.typ)
	assert.False(t, got.command.Valid)
	assert.Empty(t, healthCheckRuntimeConfig(app, got).command, "and so no Docker HEALTHCHECK either")
}

func TestCommandArgv(t *testing.T) {
	assert.Equal(t, []string{"sh", "-c", "bundle exec sidekiq -q default"}, commandArgv("bundle exec sidekiq -q default"))
	// Passed through whole: quoting, pipes and FOO=bar are the shell's business.
	assert.Equal(t, []string{"sh", "-c", `FOO=bar run --name "a b" | tee out`}, commandArgv(`FOO=bar run --name "a b" | tee out`))
}
