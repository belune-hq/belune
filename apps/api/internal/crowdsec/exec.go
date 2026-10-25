// Package crowdsec drives the CrowdSec local API that the `crowdsec` compose
// service runs, and decides what Belune does with it.
//
// Belune talks to it by running `cscli` inside that container, the same way the
// capability prober runs `caddy list-modules` in Caddy's. That is deliberate: the
// local API has two kinds of credential, a bouncer key that can only read the ban
// list and a machine password that can write it, and exec means Belune never holds
// the second one. The only secret Belune stores is the read-only bouncer key, and
// it only exists to be handed to Caddy.
package crowdsec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/belune-hq/belune/internal/runtime"
)

const (
	// SystemLabel is the value of the belune-system label on the CrowdSec
	// container. The log collector ignores system labels it has no source id for,
	// and resolveCaddyContainer looks for "caddy" only, so a second value is inert
	// to both.
	SystemLabel = "crowdsec"

	// APIURL is where Caddy reaches the local API: the compose service name on the
	// project network. Not published to the host.
	APIURL = "http://crowdsec:8080"

	// execTimeout bounds one cscli call. A call is ~100ms against a healthy API; a
	// hang means the container is unwell, and the answer is "unavailable".
	execTimeout = 15 * time.Second

	// maxOutput bounds what one call may return. A full decision list is a few
	// hundred KB; this only stops a runaway.
	maxOutput = 8 << 20

	// maxParallel bounds concurrent execs. Each is a process start inside the
	// container, and the status page, the reconciler and the evaluator can all
	// ask at once.
	maxParallel = 4
)

// ErrUnavailable means the CrowdSec container could not be reached: it is absent,
// stopped, or the runtime would not exec into it. It is not "CrowdSec said no".
var ErrUnavailable = errors.New("the CrowdSec container is not running")

// CommandError is a cscli call that ran and failed.
type CommandError struct {
	// Command is the subcommand, without arguments: arguments can carry an address
	// or a free-text reason and are not worth echoing into a log line.
	Command  string
	ExitCode int
	Stderr   string
}

func (e *CommandError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.ExitCode)
	}
	return fmt.Sprintf("cscli %s: %s", e.Command, msg)
}

// Runtime is the slice of the container runtime this package uses.
type Runtime interface {
	ListSystemContainers(ctx context.Context) ([]runtime.ContainerInfo, error)
	ContainerExec(ctx context.Context, containerName string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

// Runner runs one cscli invocation.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout string, err error)
}

// ContainerRunner runs cscli inside the CrowdSec container.
type ContainerRunner struct {
	rt  Runtime
	sem chan struct{}
}

func NewContainerRunner(rt Runtime) *ContainerRunner {
	return &ContainerRunner{rt: rt, sem: make(chan struct{}, maxParallel)}
}

// Run executes `cscli <args>` and returns its stdout. A non-zero exit is a
// *CommandError; a container that is not there is ErrUnavailable.
func (r *ContainerRunner) Run(ctx context.Context, args ...string) (string, error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	name, err := r.container(ctx)
	if err != nil {
		return "", err
	}

	var stdout, stderr limitedBuffer
	exit, err := r.rt.ContainerExec(ctx, name, append([]string{"cscli"}, args...), nil, &stdout, &stderr)
	if err != nil {
		// Docker reports a stopped or removed container as an exec error; either
		// way nothing answered.
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if exit != 0 {
		return "", &CommandError{Command: subcommand(args), ExitCode: exit, Stderr: stderr.String()}
	}
	return stdout.String(), nil
}

// container finds the running CrowdSec container by its system label. By label and
// not by name: Compose derives container names from the project directory, which
// is /opt/belune on a real install and infra/ in a checkout.
func (r *ContainerRunner) container(ctx context.Context) (string, error) {
	containers, err := r.rt.ListSystemContainers(ctx)
	if err != nil {
		return "", fmt.Errorf("listing system containers: %w", err)
	}
	var fallback string
	for _, c := range containers {
		if c.Labels["belune-system"] != SystemLabel || c.Name == "" {
			continue
		}
		if strings.HasPrefix(c.Status, "Up") {
			return c.Name, nil
		}
		fallback = c.Name
	}
	if fallback != "" {
		// Exec into a stopped container fails with a clear error of its own.
		return fallback, nil
	}
	return "", ErrUnavailable
}

// subcommand names the cscli verb for an error message: the leading words up to the
// first flag.
func subcommand(args []string) string {
	var words []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
		if len(words) == 2 {
			break
		}
	}
	return strings.Join(words, " ")
}

// limitedBuffer is a bytes.Buffer that stops growing at maxOutput. It reports every
// write as accepted so the exec stream is drained rather than stalled.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := maxOutput - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
