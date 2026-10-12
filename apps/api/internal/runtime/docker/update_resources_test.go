package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestUpdateResources_SendsMemorySwapWithMemory is the regression test for a
// live memory update that never worked.
//
// The daemon refuses an update that sets Memory without MemorySwap on a
// container that already has one — which every running container does:
// "Memory limit should be smaller than already set memoryswap limit, update
// the memoryswap at the same time". Nothing surfaced it because the only
// caller logs the failure and carries on, the limit having been persisted
// either way; the operator saw a success and an unchanged container.
//
// 2x matches what Docker itself defaults MemorySwap to when a container is
// CREATED with only Memory set, so a container limited by a live update ends
// up in the same state as one recreated with the same limit.
func TestUpdateResources_SendsMemorySwapWithMemory(t *testing.T) {
	const mem = 268435456 // 256 MiB

	got := updateResources(0.5, mem)

	assert.Equal(t, int64(mem), got.Memory)
	assert.Equal(t, int64(mem*2), got.MemorySwap,
		"Memory without MemorySwap is refused by the daemon for any running container")
	assert.Equal(t, int64(500000000), got.NanoCPUs, "0.5 cores in nanocpus")
}

// TestUpdateResources_ZeroLeavesALimitAlone pins the other half: 0 means "do
// not touch this limit", which is how an unlimited container stays unlimited.
// Sending MemorySwap for a zero Memory would be a limit the caller never asked
// for.
func TestUpdateResources_ZeroLeavesALimitAlone(t *testing.T) {
	assert.Equal(t, int64(0), updateResources(0, 0).Memory)
	assert.Equal(t, int64(0), updateResources(0, 0).MemorySwap)
	assert.Equal(t, int64(0), updateResources(0, 0).NanoCPUs)

	// One set, one not: each is independent.
	cpuOnly := updateResources(1, 0)
	assert.Equal(t, int64(1e9), cpuOnly.NanoCPUs)
	assert.Equal(t, int64(0), cpuOnly.Memory)
	assert.Equal(t, int64(0), cpuOnly.MemorySwap, "no memory limit means no swap limit either")

	memOnly := updateResources(0, 1<<20)
	assert.Equal(t, int64(0), memOnly.NanoCPUs)
	assert.Equal(t, int64(1<<21), memOnly.MemorySwap)
}
