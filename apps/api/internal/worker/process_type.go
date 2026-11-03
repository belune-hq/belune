package worker

import (
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

// commandArgv turns a process type's command string into the container's Cmd.
//
// An empty (or all-whitespace) command MUST return nil: "" means "leave the
// image's own CMD alone", and Docker treats a nil Cmd exactly that way while
// []string{""} is an empty program that fails to start. The backfilled web type
// has command = ” and every existing install depends on this returning nil.
//
// A command is run as `sh -c <command>`, the same as a scheduled task
// (scheduled_task.go), so what an operator types in a text field behaves as they
// expect: environment expansion, &&, pipes, FOO=bar cmd. A whitespace split would
// quietly mangle anything quoted.
//
// ⚠️ The cost, which is different from a scheduled task's because this is a
// long-running container rather than a one-shot exec:
//   - Signals. For a simple command most shells (dash, busybox) exec it, so it
//     becomes PID 1 and SIGTERM reaches it. For a pipeline or a compound command
//     the shell stays PID 1 and does not forward SIGTERM, so `docker stop` waits
//     out its grace period and SIGKILLs: a queue worker is killed mid-job. There
//     is deliberately no signal-forwarding wrapper here.
//   - The image needs a /bin/sh. A distroless or scratch image has none.
//   - The runtime has no Entrypoint override, so on an image that declares an
//     ENTRYPOINT these three words are passed to it as arguments rather than run.
func commandArgv(command string) []string {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	return []string{"sh", "-c", command}
}

// resolvedHealth is the health check a process type actually runs with.
type resolvedHealth struct {
	typ     string // "http", "command" or "none"
	command pgtype.Text
}

// resolveHealthCheck is the ONE place the health-check rule lives; both the
// Docker HEALTHCHECK (healthCheckRuntimeConfig) and the deploy gate (verifyHealth,
// verifyCommandHealth) read its result, because two copies of a three-line rule is
// how the http-inheritance bug ships later.
//
//	process type sets a type -> use it
//	else routable            -> inherit the application's
//	else                     -> 'none'
//
// ⚠️ The last line is load-bearing: a non-routable type must NEVER inherit 'http'.
// http is a one-shot readiness gate the control plane runs over the network against
// a port, and a worker listens on nothing, so inheriting it would make every worker
// permanently unhealthy. 'command' is the mechanism meant for workers, and it must be
// set on the type explicitly.
//
// A type's own health_check_command, when NULL, inherits the application's, which
// is what the column means. HTTP knobs (path, container_port, expect_status) and
// the Docker timing knobs stay application-level and are not resolved here.
func resolveHealthCheck(app generated.Application, pt generated.ApplicationProcessType) resolvedHealth {
	if pt.HealthCheckType.Valid && pt.HealthCheckType.String != "" {
		cmd := app.HealthCheckCommand
		if pt.HealthCheckCommand.Valid {
			cmd = pt.HealthCheckCommand
		}
		return resolvedHealth{typ: pt.HealthCheckType.String, command: cmd}
	}
	if pt.Routable {
		return resolvedHealth{typ: app.HealthCheckType, command: app.HealthCheckCommand}
	}
	return resolvedHealth{typ: "none"}
}
