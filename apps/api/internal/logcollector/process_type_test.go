package logcollector_test

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/logcollector"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/testutil"
)

// perContainerRuntime serves a separate stream per container id, once each.
type perContainerRuntime struct {
	*testutil.MockContainerRuntime
	mu      sync.Mutex
	streams map[string][]string
}

func (r *perContainerRuntime) ContainerLogsSince(_ context.Context, id string, _ time.Time) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lines, ok := r.streams[id]
	if !ok {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	delete(r.streams, id)
	return makeDockerStdoutStream(lines...), nil
}

func newUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	require.NoError(t, u.Scan(uuid.NewString()))
	return u
}

// The process type is written to every line from the container's label, an
// invalid label is stored as NULL, and -- the COPY-fails-whole regression -- a
// forged label does not discard the batch it rides in. A NUL byte is the sharpest
// forgery: Postgres rejects it in text, so the unvalidated value fails the COPY.
func TestCollector_StoresAValidatedProcessType(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })

	app := seedCollectorApp(t)
	appID := formatUUID(app.ID)
	dbID := newUUID(t)

	cases := []struct {
		id     string
		labels map[string]string
		lines  []string
	}{
		{"ctr-queue", map[string]string{"application-id": appID, runtime.LabelProcessType: "queue"}, []string{"q1", "q2"}},
		{"ctr-forged", map[string]string{"application-id": appID, runtime.LabelProcessType: "bad\x00type"}, []string{"f1", "f2"}},
		{"ctr-upper", map[string]string{"application-id": appID, runtime.LabelProcessType: "Queue"}, []string{"u1"}},
		{"ctr-legacy", map[string]string{"application-id": appID}, []string{"l1"}},
	}
	var list []runtime.ContainerInfo
	streams := map[string][]string{}
	for _, c := range cases {
		list = append(list, runtime.ContainerInfo{ID: c.id, Name: c.id, Status: "running", Labels: c.labels})
		streams[c.id] = c.lines
	}
	// A database container, which has no process type even if a label is present.
	list = append(list, runtime.ContainerInfo{ID: "ctr-db", Name: "db", Status: "running",
		Labels: map[string]string{"database-id": formatUUID(dbID), runtime.LabelProcessType: "queue"}})
	streams["ctr-db"] = []string{"d1"}

	rt := &perContainerRuntime{MockContainerRuntime: &testutil.MockContainerRuntime{ListContainers_: list}, streams: streams}
	c := logcollector.New(rt, testQueries, startMiniredis(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	count := func() int {
		var n int
		require.NoError(t, testPool.QueryRow(context.Background(), `SELECT COUNT(*) FROM container_logs`).Scan(&n))
		return n
	}
	assert.Eventually(t, func() bool { return count() == 7 }, 8*time.Second, 50*time.Millisecond,
		"every line must land, including the forged container's")

	typeOf := func(container string) []string {
		rows, err := testPool.Query(context.Background(),
			`SELECT COALESCE(process_type, '<null>') FROM container_logs WHERE container_id = $1`, container)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			require.NoError(t, rows.Scan(&s))
			out = append(out, s)
		}
		return out
	}
	assert.Equal(t, []string{"queue", "queue"}, typeOf("ctr-queue"))
	assert.Equal(t, []string{"<null>", "<null>"}, typeOf("ctr-forged"), "a forged label is stored as unknown")
	assert.Equal(t, []string{"<null>"}, typeOf("ctr-upper"), "the rule is the lowercase one migration 000079 enforces")
	assert.Equal(t, []string{"<null>"}, typeOf("ctr-legacy"), "a container with no label is unknown")
	assert.Equal(t, []string{"<null>"}, typeOf("ctr-db"), "a database has no process type")
}
