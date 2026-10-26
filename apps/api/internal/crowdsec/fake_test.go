package crowdsec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeRunner stands in for cscli. Replies are matched on the leading words of the
// command ("decisions add"), the longest match wins, and every call is recorded
// with its full argument list.
type fakeRunner struct {
	mu      sync.Mutex
	calls   [][]string
	replies map[string]fakeReply
}

type fakeReply struct {
	stdout string
	err    error
	// each, when set, is called for every match and its result used instead.
	each func(args []string) (string, error)
}

func newFakeRunner() *fakeRunner { return &fakeRunner{replies: map[string]fakeReply{}} }

func (f *fakeRunner) on(prefix, stdout string, err error) *fakeRunner {
	f.replies[prefix] = fakeReply{stdout: stdout, err: err}
	return f
}

func (f *fakeRunner) onFunc(prefix string, fn func(args []string) (string, error)) *fakeRunner {
	f.replies[prefix] = fakeReply{each: fn}
	return f
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()

	best, bestLen := "", -1
	for prefix := range f.replies {
		if strings.HasPrefix(strings.Join(args, " ")+" ", prefix+" ") && len(prefix) > bestLen {
			best, bestLen = prefix, len(prefix)
		}
	}
	if bestLen < 0 {
		return "", &CommandError{Command: subcommand(args), ExitCode: 1, Stderr: "fakeRunner: no reply for " + strings.Join(args, " ")}
	}
	r := f.replies[best]
	if r.each != nil {
		return r.each(args)
	}
	return r.stdout, r.err
}

// called returns every recorded call whose leading words match prefix.
func (f *fakeRunner) called(prefix string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " ")+" ", prefix+" ") {
			out = append(out, c)
		}
	}
	return out
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}
