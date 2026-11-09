package instances_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

// eventFake holds the ONE row the live generation owns and answers the three event
// keys the way the SQL does: exact container id, exact stored name, type + idx.
type eventFake struct{ row generated.ApplicationInstance }

func (f eventFake) GetInstanceByContainerID(_ context.Context, a generated.GetInstanceByContainerIDParams) (generated.ApplicationInstance, error) {
	if a.ContainerID.Valid && a.ContainerID.String == f.row.ContainerID.String {
		return f.row, nil
	}
	return generated.ApplicationInstance{}, pgx.ErrNoRows
}
func (f eventFake) GetInstanceByContainerName(_ context.Context, a generated.GetInstanceByContainerNameParams) (generated.ApplicationInstance, error) {
	if a.ContainerName == f.row.ContainerName {
		return f.row, nil
	}
	return generated.ApplicationInstance{}, pgx.ErrNoRows
}
func (f eventFake) GetInstanceByProcessTypeAndIdx(_ context.Context, a generated.GetInstanceByProcessTypeAndIdxParams) (generated.ApplicationInstance, error) {
	if a.Name == "web" && a.Idx == f.row.Idx {
		return f.row, nil
	}
	return generated.ApplicationInstance{}, pgx.ErrNoRows
}

func uuidOf(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

func liveRow(dep pgtype.UUID) generated.ApplicationInstance {
	return generated.ApplicationInstance{
		Idx: 1, ContainerName: "app-web-1-bbbb2222",
		ContainerID: pgtype.Text{String: "new-id", Valid: true}, DeploymentID: dep,
	}
}

func labels(dep pgtype.UUID) map[string]string {
	s := ""
	if dep.Valid {
		b := dep.Bytes
		s = fmtUUID(b)
	}
	return map[string]string{runtime.LabelProcessType: "web", runtime.LabelInstanceIdx: "1", runtime.LabelDeploymentID: s}
}

func fmtUUID(b [16]byte) string {
	u := pgtype.UUID{Bytes: b, Valid: true}
	v, _ := u.Value()
	return v.(string)
}

// The race a roll opens: the draining generation's late 'die' names ITS container
// (different id, different suffixed name) but carries the same type + idx labels as
// the live one. It must not resolve to the live row.
func TestResolveForContainer_AStaleGenerationsEventDoesNotLandOnTheLiveRow(t *testing.T) {
	live, old := uuidOf(2), uuidOf(1)
	q := eventFake{row: liveRow(live)}
	ev := runtime.ContainerEvent{ContainerID: "old-id", ContainerName: "app-web-1-aaaa1111", Status: "die", Labels: labels(old)}

	_, found, err := instances.ResolveForContainer(context.Background(), q, pgtype.UUID{}, ev)
	require.NoError(t, err)
	assert.False(t, found, "an event from the old generation is not the live instance's")
}

func TestResolveForContainer_TheLiveGenerationStillResolvesOnEveryKey(t *testing.T) {
	live := uuidOf(2)
	q := eventFake{row: liveRow(live)}
	for name, ev := range map[string]runtime.ContainerEvent{
		"by id":     {ContainerID: "new-id"},
		"by name":   {ContainerName: "app-web-1-bbbb2222"},
		"by labels": {ContainerID: "x", ContainerName: "y", Labels: labels(live)},
	} {
		_, found, err := instances.ResolveForContainer(context.Background(), q, pgtype.UUID{}, ev)
		require.NoError(t, err, name)
		assert.True(t, found, name)
	}
}

// deployment_id is nullable by design: a backfilled row or a restored database has
// none, and a container's label must still find it.
func TestResolveForContainer_ARowWithNoRecordedDeploymentStillMatchesByLabels(t *testing.T) {
	q := eventFake{row: liveRow(pgtype.UUID{})}
	ev := runtime.ContainerEvent{ContainerID: "x", ContainerName: "y", Labels: labels(uuidOf(9))}
	_, found, err := instances.ResolveForContainer(context.Background(), q, pgtype.UUID{}, ev)
	require.NoError(t, err)
	assert.True(t, found)
}

// A container created before the labels existed has none; it keeps resolving by name.
func TestResolveForContainer_ALegacyNamedContainerResolvesByItsStoredName(t *testing.T) {
	row := liveRow(pgtype.UUID{})
	row.ContainerName = "my-app-pcr-calculator-5586176d"
	_, found, err := instances.ResolveForContainer(context.Background(), eventFake{row: row}, pgtype.UUID{},
		runtime.ContainerEvent{ContainerName: "my-app-pcr-calculator-5586176d"})
	require.NoError(t, err)
	assert.True(t, found)
}
