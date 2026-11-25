package worker_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

func runVolumeCleanup(t *testing.T, rt *testutil.MockContainerRuntime) {
	t.Helper()
	h := newTestHandler(rt, nil)
	require.NoError(t, h.HandleCleanupTask(context.Background(), asynq.NewTask("cleanup", []byte(`{"actions":["volumes"],"scheduled":false}`))))
}

func TestVolumePruneClaimsIncludeEveryOwner(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, testutil.TruncateAll(ctx, testPool))
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, testPool) })
	app, _ := seedApp(t)
	db := seedDatabase(t)
	_, err := testPool.Exec(ctx, `INSERT INTO adopted_volumes (name) VALUES ($1)`, "compose_pgdata")
	require.NoError(t, err)
	for _, row := range []struct{ name, external string }{{"managed", ""}, {"adopted", "compose_pgdata"}} {
		_, err := testQueries.CreateApplicationVolume(ctx, generated.CreateApplicationVolumeParams{
			ApplicationID: app.ID, Name: row.name, MountPath: "/" + row.name,
			ExternalName: pgtype.Text{String: row.external, Valid: row.external != ""},
		})
		require.NoError(t, err)
	}
	rt := &testutil.MockContainerRuntime{}
	runVolumeCleanup(t, rt)
	require.Len(t, rt.PruneVolumeCalls, 1)
	assert.ElementsMatch(t, []string{naming.VolumeNameFor(uuidString(app.ID), "managed", ""), "compose_pgdata", db.Slug + "-vol"}, rt.PruneVolumeCalls[0])
}

// A real empty installation is not a failed claims lookup. Foreign orphans
// must still be reclaimable there, including during a manual cleanup.
func TestVolumePruneRunsWithSuccessfullyEmptyClaims(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, testutil.TruncateAll(ctx, testPool))
	rt := &testutil.MockContainerRuntime{}
	runVolumeCleanup(t, rt)
	require.Len(t, rt.PruneVolumeCalls, 1)
	assert.Empty(t, rt.PruneVolumeCalls[0])
}

func TestVolumePruneFailsClosedOnEveryClaimsQuery(t *testing.T) {
	for _, table := range []string{"application_volumes", "databases", "adopted_volumes"} {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()
			hidden := table + "_claims_failure"
			_, err := testPool.Exec(ctx, "ALTER TABLE "+pgx.Identifier{table}.Sanitize()+" RENAME TO "+pgx.Identifier{hidden}.Sanitize())
			require.NoError(t, err)
			defer func() {
				_, err := testPool.Exec(ctx, "ALTER TABLE "+pgx.Identifier{hidden}.Sanitize()+" RENAME TO "+pgx.Identifier{table}.Sanitize())
				require.NoError(t, err)
			}()
			rt := &testutil.MockContainerRuntime{}
			runVolumeCleanup(t, rt)
			assert.Empty(t, rt.PruneVolumeCalls, "failed claims query must skip pruning, never pass an empty keep-list")
		})
	}
}

// Every production data-volume creator needs a corresponding owner source in
// collectVolumeClaims. Adding a new owner/creator must force an explicit review
// here, instead of silently making that resource eligible for deletion.
func TestVolumeClaimsInventoryCoversDataVolumeCreators(t *testing.T) {
	callers := map[string]bool{}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		tree, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(tree, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "CreateDataVolume" {
				callers[filepath.ToSlash(path)] = true
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"../worker/deploy_task.go": true, "../worker/provision_db_task.go": true}, callers,
		"new data-volume creator: teach collectVolumeClaims its owner before changing this inventory")
}
