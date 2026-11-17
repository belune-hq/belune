package logcollector_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/logcollector"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/testutil"
)

// The replica index is written to every line from the container's label. A label that is
// not a plain small number is stored as NULL, and -- the COPY-fails-whole regression -- a
// forged one does not discard the batch it rides in. The forgeries here are the ones a
// lenient parse would accept (sign, padding, a hex or float spelling) or that overflow.
func TestCollector_StoresAValidatedInstanceIdx(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })

	app := seedCollectorApp(t)
	appID := formatUUID(app.ID)
	dbID := newUUID(t)

	label := func(v string) map[string]string {
		return map[string]string{"application-id": appID, runtime.LabelProcessType: "web", runtime.LabelInstanceIdx: v}
	}
	cases := []struct {
		id     string
		labels map[string]string
		want   string
	}{
		{"ctr-0", label("0"), "0"},
		{"ctr-3", label("3"), "3"},
		{"ctr-neg", label("-1"), "<null>"},
		{"ctr-plus", label("+2"), "<null>"},
		{"ctr-pad", label(" 2"), "<null>"},
		{"ctr-hex", label("0x2"), "<null>"},
		{"ctr-float", label("1.5"), "<null>"},
		{"ctr-huge", label("99999999999999999999"), "<null>"},
		{"ctr-big", label("1001"), "<null>"},
		{"ctr-nul", label("1\x00"), "<null>"},
		{"ctr-empty", label(""), "<null>"},
		{"ctr-legacy", map[string]string{"application-id": appID}, "<null>"},
	}
	var list []runtime.ContainerInfo
	streams := map[string][]string{}
	for _, c := range cases {
		list = append(list, runtime.ContainerInfo{ID: c.id, Name: c.id, Status: "running", Labels: c.labels})
		streams[c.id] = []string{c.id + "-a", c.id + "-b"}
	}
	// A database has no replica index even if a label is present.
	list = append(list, runtime.ContainerInfo{ID: "ctr-db", Name: "db", Status: "running",
		Labels: map[string]string{"database-id": formatUUID(dbID), runtime.LabelInstanceIdx: "1"}})
	streams["ctr-db"] = []string{"d1"}

	rt := &perContainerRuntime{MockContainerRuntime: &testutil.MockContainerRuntime{ListContainers_: list}, streams: streams}
	c := logcollector.New(rt, testQueries, startMiniredis(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	want := len(cases)*2 + 1
	assert.Eventually(t, func() bool {
		var n int
		require.NoError(t, testPool.QueryRow(context.Background(), `SELECT COUNT(*) FROM container_logs`).Scan(&n))
		return n == want
	}, 8*time.Second, 50*time.Millisecond, "every line must land, including the forged containers'")

	for _, c := range cases {
		rows, err := testPool.Query(context.Background(),
			`SELECT COALESCE(instance_idx::text, '<null>') FROM container_logs WHERE container_id = $1`, c.id)
		require.NoError(t, err)
		var got []string
		for rows.Next() {
			var s string
			require.NoError(t, rows.Scan(&s))
			got = append(got, s)
		}
		rows.Close()
		assert.Equal(t, []string{c.want, c.want}, got, c.id)
	}
	var dbIdx *int32
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT instance_idx FROM container_logs WHERE container_id = 'ctr-db'`).Scan(&dbIdx))
	assert.Nil(t, dbIdx, "a database has no replica index")
}
