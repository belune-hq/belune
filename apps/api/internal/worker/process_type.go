package worker

import (
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/store/generated"
)

// commandOverride turns a process type's command string into the container's
// Entrypoint and Cmd.
//
// An empty (or all-whitespace) command MUST return nil for BOTH: "" means "leave
// the image alone", and Docker treats nil as "inherit the image's CMD and
// ENTRYPOINT" while []string{""} is an empty program that fails to start. The
// backfilled web type has command = ” and every existing install depends on this
// leaving the container config byte-identical to before.
//
// A command is run as `sh -c <command>`, the same as a scheduled task
// (scheduled_task.go), so what an operator types in a text field behaves as they
// expect: environment expansion, &&, pipes, FOO=bar cmd. A whitespace split would
// quietly mangle anything quoted.
//
// ⛔ The entrypoint is CLEARED (empty, non-nil) whenever a command is set. It was
// measured that without this the command silently runs NOTHING on the images most
// operators build: a Railpack image carries ENTRYPOINT ["/bin/bash","-c"], and
// `bash -c sh -c "<cmd>"` executes `sh` with the rest as positional parameters and
// discards the command. Buildpacks images use /cnb/lifecycle/launcher, which
// interprets Cmd on its own terms. Clearing it makes the command behave the same
// whatever built the image. This is a mechanism, not a configurable field.
//
// ⚠️ Remaining costs, different from a scheduled task's because this is a
// long-running container rather than a one-shot exec:
//   - Signals. For a simple command most shells (dash, busybox) exec it, so it
//     becomes PID 1 and SIGTERM reaches it. For a pipeline or a compound command
//     the shell stays PID 1 and does not forward SIGTERM, so `docker stop` waits
//     out its grace period and SIGKILLs: a queue worker is killed mid-job. There
//     is deliberately no signal-forwarding wrapper here.
//   - The image needs a /bin/sh. A distroless or scratch image has none.
func commandOverride(command string) (entrypoint, cmd []string) {
	if strings.TrimSpace(command) == "" {
		return nil, nil
	}
	return []string{}, []string{"sh", "-c", command}
}

// resolvedHealth is the health check a process type actually runs with.
type resolvedHealth struct {
	typ     string // "http", "command" or "none"
	command pgtype.Text
}

// resolveHealthCheck reads the one health-check rule, which lives in
// instances.ResolveHealth so the API reports exactly what this worker runs.
func resolveHealthCheck(app generated.Application, pt generated.ApplicationProcessType) resolvedHealth {
	h := instances.ResolveHealth(app, pt)
	return resolvedHealth{typ: h.Type, command: h.Command}
}
