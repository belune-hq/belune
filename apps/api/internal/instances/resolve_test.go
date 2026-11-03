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
