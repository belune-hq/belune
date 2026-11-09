package instances_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/store/generated"
)

type fake struct {
	all, routable []generated.ApplicationInstance
}

func (f fake) ListInstancesByApplication(context.Context, pgtype.UUID) ([]generated.ApplicationInstance, error) {
	return f.all, nil
}
func (f fake) ListRoutableInstancesByApplication(context.Context, pgtype.UUID) ([]generated.ApplicationInstance, error) {
	return f.routable, nil
}

func inst(idx int32, name, status string) generated.ApplicationInstance {
	return generated.ApplicationInstance{Idx: idx, ContainerName: name, Status: status}
}

// No row must be an explicit error: an empty name would flow into a Docker call
// and fail somewhere unrecognisable, which is how a silent 502 ships.
func TestResolve_NoInstanceRowFailsLoudly(t *testing.T) {
	_, err := instances.ResolveOne(context.Background(), fake{}, pgtype.UUID{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, instances.ErrNoInstance))

	_, err = instances.ResolveAll(context.Background(), fake{}, pgtype.UUID{})
	assert.True(t, errors.Is(err, instances.ErrNoInstance))
}

func TestResolveOne_LowestRunningIdx(t *testing.T) {
	q := fake{routable: []generated.ApplicationInstance{inst(1, "a", "stopped"), inst(2, "b", "running"), inst(3, "c", "running")}}
	got, err := instances.ResolveOne(context.Background(), q, pgtype.UUID{})
	require.NoError(t, err)
	assert.Equal(t, "b", got.ContainerName)
}

// Instance status is not driven until A4, so a freshly created app reads
// 'pending' while its container runs; logs and exec must still resolve.
func TestResolveOne_FallsBackToLowestIdxWhenNoneRecordedRunning(t *testing.T) {
	q := fake{routable: []generated.ApplicationInstance{inst(1, "a", "pending"), inst(2, "b", "pending")}}
	got, err := instances.ResolveOne(context.Background(), q, pgtype.UUID{})
	require.NoError(t, err)
	assert.Equal(t, "a", got.ContainerName)
}

func TestResolveAll_KeepsTheQueryOrder(t *testing.T) {
	q := fake{all: []generated.ApplicationInstance{inst(1, "a", "running"), inst(2, "b", "running")}}
	got, err := instances.ResolveAll(context.Background(), q, pgtype.UUID{})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].ContainerName)
	assert.Equal(t, "b", got[1].ContainerName)
}

type typeFake struct {
	rows []generated.ApplicationInstance
}

func (f typeFake) ListInstancesByApplicationAndProcessType(context.Context, generated.ListInstancesByApplicationAndProcessTypeParams) ([]generated.ApplicationInstance, error) {
	return f.rows, nil
}

func TestResolveType_PrefersRunningThenLowestIdx(t *testing.T) {
	got, err := instances.ResolveType(context.Background(), typeFake{rows: []generated.ApplicationInstance{inst(1, "a", "stopped"), inst(2, "b", "running")}}, pgtype.UUID{}, "queue")
	require.NoError(t, err)
	assert.Equal(t, "b", got.ContainerName)

	got, err = instances.ResolveType(context.Background(), typeFake{rows: []generated.ApplicationInstance{inst(1, "a", "pending"), inst(2, "b", "pending")}}, pgtype.UUID{}, "queue")
	require.NoError(t, err)
	assert.Equal(t, "a", got.ContainerName, "a stopped container's logs must stay readable")
}

// An unknown type is the caller's mistake, distinct from a missing row.
func TestResolveType_UnknownTypeIsItsOwnError(t *testing.T) {
	_, err := instances.ResolveType(context.Background(), typeFake{}, pgtype.UUID{}, "nope")
	assert.True(t, errors.Is(err, instances.ErrNoSuchProcessType))
	assert.False(t, errors.Is(err, instances.ErrNoInstance))
}

// The health rule's four cases, which the API now reports and the deploy worker runs.
func TestResolveHealth_FourCases(t *testing.T) {
	app := generated.Application{HealthCheckType: "http", HealthCheckPath: pgtype.Text{String: "/up", Valid: true}}
	set := func(kind string) pgtype.Text { return pgtype.Text{String: kind, Valid: true} }

	got := instances.ResolveHealth(app, generated.ApplicationProcessType{Routable: true})
	assert.Equal(t, instances.Health{Type: "http", Source: instances.HealthSourceInherited}, got, "routable, unset: inherits the application's")

	got = instances.ResolveHealth(app, generated.ApplicationProcessType{Routable: true, HealthCheckType: set("none")})
	assert.Equal(t, "none", got.Type)
	assert.Equal(t, instances.HealthSourceSet, got.Source, "routable, set: the type's own")

	got = instances.ResolveHealth(app, generated.ApplicationProcessType{})
	assert.Equal(t, instances.Health{Type: "none", Source: instances.HealthSourceDefault}, got, "⚠️ a non-routable type must never inherit http")

	got = instances.ResolveHealth(app, generated.ApplicationProcessType{HealthCheckType: set("command")})
	assert.Equal(t, "command", got.Type)
	assert.Equal(t, instances.HealthSourceSet, got.Source, "non-routable, set: the type's own")
}

// D1: names minted from now on carry a suffix, but a stored name is data. The install
// still runs a routable container called after the pre-0.3.0 slug beside rows minted
// later; every resolver must hand back exactly what is stored.
func TestResolvers_ReturnStoredNamesUnchangedWhateverTheirShape(t *testing.T) {
	legacy := "my-app-pcr-calculator-5586176d"
	minted := "my-app-pcr-calculator-5586176d-worker-1-0f9b521e"
	web := inst(1, legacy, "running")
	worker := inst(1, minted, "running")
	q := fake{all: []generated.ApplicationInstance{web, worker}, routable: []generated.ApplicationInstance{web}}
	ctx := context.Background()

	one, err := instances.ResolveOne(ctx, q, pgtype.UUID{})
	require.NoError(t, err)
	assert.Equal(t, legacy, one.ContainerName)

	all, err := instances.ResolveAll(ctx, q, pgtype.UUID{})
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, legacy, all[0].ContainerName)
	assert.Equal(t, minted, all[1].ContainerName)

	names, err := instances.RoutableContainerNames(ctx, q, pgtype.UUID{})
	require.NoError(t, err)
	assert.Equal(t, []string{legacy}, names)
}

// Inert is the one definition of "configured but checks nothing": http with no path.
// Type stays "http" (the operator chose it); only Inert says nothing will be probed.
func TestResolveHealth_InertOnlyForHTTPWithNoPath(t *testing.T) {
	path := func(p pgtype.Text) generated.Application {
		return generated.Application{HealthCheckType: "http", HealthCheckPath: p}
	}
	routable := generated.ApplicationProcessType{Routable: true}

	for name, p := range map[string]pgtype.Text{"NULL": {}, "empty": {String: "", Valid: true}} {
		got := instances.ResolveHealth(path(p), routable)
		assert.Equal(t, "http", got.Type, "%s path: never flattened to none", name)
		assert.True(t, got.Inert, name)
		assert.Equal(t, instances.HealthInertNoPath, got.Reason, name)
	}

	got := instances.ResolveHealth(path(pgtype.Text{String: "/up", Valid: true}), routable)
	assert.False(t, got.Inert, "http with a path is probed")
	assert.Empty(t, got.Reason)

	cmd := generated.Application{HealthCheckType: "command"}
	assert.False(t, instances.ResolveHealth(cmd, routable).Inert, "command is not inert")
	none := generated.Application{HealthCheckType: "none"}
	assert.False(t, instances.ResolveHealth(none, routable).Inert, "an explicit none is a choice, not a gap")
	assert.False(t, instances.ResolveHealth(path(pgtype.Text{}), generated.ApplicationProcessType{}).Inert, "a worker resolves to none")

	// A type that sets http itself, over an application with no path, is inert too.
	set := generated.ApplicationProcessType{Routable: true, HealthCheckType: pgtype.Text{String: "http", Valid: true}}
	assert.True(t, instances.ResolveHealth(generated.Application{HealthCheckType: "none"}, set).Inert)
}
