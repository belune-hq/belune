package handler_test

import (
	"context"
	"fmt"
	"github.com/belune-hq/belune/internal/service"
	"testing"
	"time"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestAdoptVolumeAdminBoundaryRetentionAndAudit(t *testing.T) {
	resetDB(t)
	auditSvc := service.NewAuditService(env.Queries)
	auditCtx, cancelAudit := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { auditSvc.Run(auditCtx); close(done) }()
	t.Cleanup(func() { cancelAudit(); <-done })
	env := testutil.SetupTestServer(env.Pool, env.Queries, auditSvc)
	t.Cleanup(func() { env.Server.Close(); _ = env.Redis.Close(); env.RedisSrv.Close() })
	admin := env.SetupAdmin(t, "admin@test.com", "password123")
	_, member := createMember(t, admin, "volume-owner@test.com")
	project := env.CreateProject(t, member, "Volumes", "volumes")
	pid := extractID(project["id"])
	app := env.CreateApplication(t, member, pid, map[string]any{"name": "Data", "type": "image", "build_type": "image", "source_image": "busybox:latest"})
	id := extractID(app["id"])
	path := fmt.Sprintf("/api/projects/%s/applications/%s/volumes", pid, id)
	request := map[string]string{"name": "data", "mount_path": "/data", "external_name": "compose_pgdata"}
	env.Runtime.ListVolumes_ = []runtime.VolumeInfo{{Name: "compose_pgdata"}}
	res := env.DoRequest(t, "POST", path+"/adopt", request, testutil.AuthHeader(member))
	require.Equal(t, 403, res.StatusCode)
	_ = res.Body.Close()
	require.Zero(t, env.Runtime.VolumeListCalls, "a member owning the project must not reach install-wide volumes")
	res = env.DoRequest(t, "POST", path+"/adopt", request, testutil.AuthHeader(admin))
	require.Equal(t, 201, res.StatusCode)
	vol := testutil.ReadJSON(t, res)
	require.Equal(t, "compose_pgdata", vol["external_name"])
	volumeID := extractID(vol["id"])
	res = env.DoRequest(t, "POST", path+"/"+volumeID+"/backups/"+id+"/restore", nil, testutil.AuthHeader(admin))
	require.Equal(t, 409, res.StatusCode)
	require.Contains(t, testutil.ReadJSON(t, res)["error"], "disabled")
	var count int
	require.Eventually(t, func() bool {
		err := env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action='adopt_volume' AND details->>'external_name'='compose_pgdata'`).Scan(&count)
		return err == nil && count == 1
	}, time.Second, 10*time.Millisecond)
	res = env.DoRequest(t, "DELETE", path+"/"+volumeID+"?delete_data=true", nil, testutil.AuthHeader(admin))
	require.Equal(t, 400, res.StatusCode)
	_ = res.Body.Close()
	res = env.DoRequest(t, "DELETE", path+"/"+volumeID, nil, testutil.AuthHeader(admin))
	require.Equal(t, 200, res.StatusCode)
	detached := testutil.ReadJSON(t, res)
	require.Contains(t, detached["warning"], "retained")
	registry, err := env.Queries.ListAdoptedVolumes(context.Background())
	require.NoError(t, err)
	require.Len(t, registry, 1)
	res = env.DoRequest(t, "GET", "/api/docker/volumes", nil, testutil.AuthHeader(admin))
	require.Equal(t, 200, res.StatusCode)
	rows := testutil.ReadJSONArray(t, res)
	require.Len(t, rows, 1)
	require.Equal(t, true, rows[0].(map[string]any)["adopted"])
	require.Nil(t, rows[0].(map[string]any)["owner"])
}
