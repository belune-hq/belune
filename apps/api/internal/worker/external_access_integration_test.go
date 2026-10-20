package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime/docker"
)

// redisPing dials addr and speaks the redis inline protocol; true means the
// server answered PONG (so the loopback host-port binding is live).
func redisPing(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		return false
	}
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	return strings.Contains(string(buf[:n]), "PONG")
}

// TestExternalAccessToggle_RealDocker verifies the Phase 6 SSH-tunnel external
// access: enabling it recreates the container with a 127.0.0.1:<port> binding
// (reachable from the host loopback), data survives the recreate, and disabling
// it removes the binding.
func TestExternalAccessToggle_RealDocker(t *testing.T) {
	if os.Getenv("BELUNE_DOCKER_INTEGRATION") == "" {
		t.Skip("set BELUNE_DOCKER_INTEGRATION=1 to run the real-Docker external-access toggle")
	}
	ctx := context.Background()
	rt, err := docker.New()
	require.NoError(t, err)
	h := newTestHandler(rt, nil)

	db := otherRedisDB(t, "none", nil) // internal-only at provision
	provisionAndCleanup(t, h, rt, db)

	redisExec := func(cmd string) (int, error) {
		return rt.ContainerExec(ctx, db.Slug, []string{"sh", "-c", "redis-cli " + cmd}, nil, nil, nil)
	}
	requireReady(t, func() bool { exit, err := redisExec("PING"); return err == nil && exit == 0 })
	_, err = redisExec("SET k 1")
	require.NoError(t, err)
	_, err = redisExec("SAVE")
	require.NoError(t, err)

	// Enable external access → recreate with a loopback host port.
	enable, _ := json.Marshal(map[string]any{"database_id": dbIDStr(db), "enable": true})
	require.NoError(t, h.HandleReconfigureDBTask(ctx, asynq.NewTask("reconfigure_db", enable)))

	got, err := testQueries.GetDatabase(ctx, db.ID)
	require.NoError(t, err)
	require.True(t, got.HostPort.Valid, "host port should be assigned when external access is enabled")
	addr := fmt.Sprintf("127.0.0.1:%d", got.HostPort.Int32)

	requireReady(t, func() bool { return redisPing(addr) })

	// Data must survive the recreate (the volume is reattached).
	var out bytes.Buffer
	_, err = rt.ContainerExec(ctx, db.Slug, []string{"sh", "-c", "redis-cli GET k"}, nil, &out, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "1", "data did not survive the external-access recreate")

	// Disable → recreate without the binding; the port should stop answering.
	disable, _ := json.Marshal(map[string]any{"database_id": dbIDStr(db), "enable": false})
	require.NoError(t, h.HandleReconfigureDBTask(ctx, asynq.NewTask("reconfigure_db", disable)))

	off, err := testQueries.GetDatabase(ctx, db.ID)
	require.NoError(t, err)
	assert.False(t, off.HostPort.Valid, "host port should be cleared when external access is disabled")

	deadline := time.Now().Add(20 * time.Second)
	for redisPing(addr) {
		if time.Now().After(deadline) {
			t.Fatal("loopback port still reachable after disabling external access")
		}
		time.Sleep(time.Second)
	}
}
