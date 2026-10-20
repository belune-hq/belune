package worker

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

func i4(v int32) pgtype.Int4 { return pgtype.Int4{Int32: v, Valid: true} }
func txt(v string) pgtype.Text {
	return pgtype.Text{String: v, Valid: v != ""}
}

func TestHealthCheckRuntimeConfig(t *testing.T) {
	// Non-command types never produce a HEALTHCHECK, whatever else is set.
	for _, typ := range []string{"http", "none", ""} {
		app := generated.Application{
			HealthCheckType:    typ,
			HealthCheckCommand: txt("curl -f localhost/health"),
		}
		if got := healthCheckRuntimeConfig(app); got.command != "" {
			t.Errorf("type %q: expected no command, got %q", typ, got.command)
		}
	}

	// A command type with everything NULL falls back to Docker's defaults.
	def := healthCheckRuntimeConfig(generated.Application{
		HealthCheckType:    "command",
		HealthCheckCommand: txt("pg_isready"),
	})
	if def.command != "pg_isready" {
		t.Errorf("command = %q", def.command)
	}
	if def.interval != defaultHealthInterval ||
		def.timeout != defaultHealthTimeout ||
		def.retries != defaultHealthRetries ||
		def.startPeriod != defaultHealthStartPeriod {
		t.Errorf("defaults not applied: %+v", def)
	}

	// A command type with 'command' selected but no command string is a no-op,
	// so a misconfigured row cannot set an empty HEALTHCHECK.
	if got := healthCheckRuntimeConfig(generated.Application{HealthCheckType: "command"}); got.command != "" {
		t.Errorf("empty command should yield no config, got %q", got.command)
	}

	// Explicit values win over the defaults.
	custom := healthCheckRuntimeConfig(generated.Application{
		HealthCheckType:               "command",
		HealthCheckCommand:            txt("redis-cli ping"),
		HealthCheckIntervalSeconds:    i4(10),
		HealthCheckTimeoutSeconds:     i4(5),
		HealthCheckRetries:            i4(2),
		HealthCheckStartPeriodSeconds: i4(15),
	})
	if custom.interval != 10*time.Second ||
		custom.timeout != 5*time.Second ||
		custom.retries != 2 ||
		custom.startPeriod != 15*time.Second {
		t.Errorf("explicit values not honoured: %+v", custom)
	}
}

func TestDeployFailureErrorsApp(t *testing.T) {
	running := generated.Application{Status: status.ApplicationRunning}
	stopped := generated.Application{Status: status.ApplicationStopped}
	inactive := generated.Application{Status: status.ApplicationInactive}

	cases := []struct {
		name string
		dc   *deployContext
		want bool
	}{
		{
			// The whole point of the reorder: build fails before cleanup, old
			// container still serving — keep the app up.
			"running app, failure before cleanup",
			&deployContext{app: running, cleanedUp: false},
			false,
		},
		{
			"running app, failure after cleanup",
			&deployContext{app: running, cleanedUp: true},
			true,
		},
		{
			// First-ever deploy has nothing running to preserve; a failure
			// should surface on the row.
			"inactive app, failure before cleanup",
			&deployContext{app: inactive, cleanedUp: false},
			true,
		},
		{
			"stopped app, failure before cleanup",
			&deployContext{app: stopped, cleanedUp: false},
			true,
		},
		{
			// Build-only never touches the container, so it never errors the app.
			"build-only on a running app",
			&deployContext{app: running, buildOnly: true},
			false,
		},
		{
			"build-only on a stopped app",
			&deployContext{app: stopped, buildOnly: true},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deployFailureErrorsApp(c.dc); got != c.want {
				t.Errorf("deployFailureErrorsApp() = %v, want %v", got, c.want)
			}
		})
	}
}
