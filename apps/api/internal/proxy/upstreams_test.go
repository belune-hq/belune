package proxy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// UpstreamURLs never sorts: the order is SQL's, and part of the route's identity.
func TestUpstreamURLs_KeepsTheCallersOrder(t *testing.T) {
	assert.Equal(t,
		[]string{"http://b:8080", "http://a:8080", "http://c:8080"},
		proxy.UpstreamURLs(8080, "b", "a", "c"))
}

// The order the route is built in comes from the query. Vacuous at one replica;
// this is the C2 guard: instances inserted out of idx order must come back idx
// ordered from SQL, because nothing downstream re-sorts them.
func TestRoutableInstances_ComeBackInIdxOrderWhateverTheInsertOrder(t *testing.T) {
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	app := seedRunningApp(t, ctx, queries)
	web, err := queries.GetProcessTypeByName(ctx, generated.GetProcessTypeByNameParams{ApplicationID: app.ID, Name: "web"})
	require.NoError(t, err)
	for _, idx := range []int32{3, 2} { // idx 1 already exists from the seed
		_, err := queries.CreateInstance(ctx, generated.CreateInstanceParams{
			ApplicationID: app.ID, ProcessTypeID: web.ID, Idx: idx,
			ContainerName: app.Slug + "-" + string(rune('0'+idx)), Status: "running",
		})
		require.NoError(t, err)
	}

	rows, err := queries.ListRoutableInstancesByApplication(ctx, app.ID)
	require.NoError(t, err)
	var idxs []int32
	for _, r := range rows {
		idxs = append(idxs, r.Idx)
	}
	assert.Equal(t, []int32{1, 2, 3}, idxs)
}
