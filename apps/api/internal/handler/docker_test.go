package handler_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

func TestDockerEndpoints_AdminReadOnly(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Seed the mock runtime with a mix of managed and foreign resources.
	env.Runtime.SystemInfo_ = &runtime.DockerSystemInfo{
		ServerVersion:     "28.5.2",
		StorageDriver:     "overlay2",
		NCPU:              4,
		Containers:        3,
		ContainersRunning: 2,
		Images:            5,
	}
	env.Runtime.SystemDiskUsage_ = &runtime.DockerDiskUsage{
		LayersSize: 1000,
		Images:     runtime.DiskUsageEntry{Count: 5, Size: 1000, Reclaimable: 400},
		Volumes:    runtime.DiskUsageEntry{Count: 2, Size: 500, Reclaimable: 100},
	}
	env.Runtime.ListAllContainers_ = []runtime.ContainerInfo{
		{ID: "abc", Name: "belune-app", Image: "app:latest", Status: "running",
			Labels:    map[string]string{"managed-by": "belune", "application-id": "not-a-real-uuid"},
			CreatedAt: time.Now()},
		{ID: "def", Name: "some-foreign", Image: "redis:7", Status: "exited",
			Labels: map[string]string{}},
	}
	env.Runtime.ListImages_ = []runtime.ImageInfo{
		{ID: "img1", RepoTags: []string{"app:latest"}, Size: 100, Dangling: false,
			Labels: map[string]string{"managed-by": "belune"}},
		{ID: "img2", RepoTags: []string{}, Size: 50, Dangling: true},
	}
	env.Runtime.ListVolumes_ = []runtime.VolumeInfo{
		{Name: "belune-vol-data", Driver: "local", Size: 200, RefCount: 1,
			Labels: map[string]string{"managed-by": "belune", "belune-data": "true"}},
		{Name: "orphan", Driver: "local", Size: 10, RefCount: 0, Labels: map[string]string{}},
	}
	env.Runtime.ListNetworks_ = []runtime.NetworkInfo{
		{ID: "net1", Name: "belune-proj", Driver: "bridge", Scope: "local",
			Labels:     map[string]string{"managed-by": "belune"},
			Containers: []runtime.NetworkContainer{{ID: "abc", Name: "belune-app", IPv4Address: "172.20.0.2/16"}}},
	}
	t.Cleanup(func() {
		env.Runtime.SystemInfo_ = nil
		env.Runtime.SystemDiskUsage_ = nil
		env.Runtime.ListAllContainers_ = nil
		env.Runtime.ListImages_ = nil
		env.Runtime.ListVolumes_ = nil
		env.Runtime.ListNetworks_ = nil
	})

	// Overview.
	resp := env.DoRequest(t, "GET", "/api/docker/overview", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	overview := testutil.ReadJSON(t, resp)
	assert.NotNil(t, overview["info"])
	assert.NotNil(t, overview["disk_usage"])
	counts, ok := overview["counts"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 2, counts["containers_running"])
	assert.EqualValues(t, 3, counts["containers_total"])
	assert.EqualValues(t, 2, counts["volumes"])

	// Containers — both managed and foreign, with managed flag set correctly.
	resp = env.DoRequest(t, "GET", "/api/docker/containers", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	containers := testutil.ReadJSONArray(t, resp)
	require.Len(t, containers, 2)
	first := containers[0].(map[string]any)
	assert.Equal(t, "belune-app", first["name"])
	assert.Equal(t, true, first["managed"])
	// The label carries an invalid UUID, so owner resolution yields nothing.
	assert.Nil(t, first["owner"])
	second := containers[1].(map[string]any)
	assert.Equal(t, false, second["managed"])

	// Images — dangling detection preserved.
	resp = env.DoRequest(t, "GET", "/api/docker/images", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	images := testutil.ReadJSONArray(t, resp)
	require.Len(t, images, 2)
	assert.Equal(t, true, images[0].(map[string]any)["managed"])
	assert.Equal(t, true, images[1].(map[string]any)["dangling"])

	// Volumes — data classification surfaced.
	resp = env.DoRequest(t, "GET", "/api/docker/volumes", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	volumes := testutil.ReadJSONArray(t, resp)
	require.Len(t, volumes, 2)
	assert.Equal(t, "data", volumes[0].(map[string]any)["kind"])

	// Networks — attached containers surfaced.
	resp = env.DoRequest(t, "GET", "/api/docker/networks", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	networks := testutil.ReadJSONArray(t, resp)
	require.Len(t, networks, 1)
	attached, ok := networks[0].(map[string]any)["containers"].([]any)
	require.True(t, ok)
	assert.Len(t, attached, 1)
}

// TestDockerImages_OwnerFromDeployment verifies platform-built images are
// attributed to their owning application via the deployments table (image_tag →
// application), independent of any Docker image labels.
func TestDockerImages_OwnerFromDeployment(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	proj := env.CreateProject(t, adminToken, "Proj", "proj")

	var projID pgtype.UUID
	require.NoError(t, projID.Scan(proj["id"].(string)))

	ctx := context.Background()
	app, err := env.Queries.CreateApplication(ctx, generated.CreateApplicationParams{
		ProjectID: projID,
		Name:      "My App",
		Slug:      "my-app",
		Type:      "git",
		BuildType: "dockerfile",
	})
	require.NoError(t, err)

	dep, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
		ApplicationID: app.ID,
		Status:        "success",
		TriggeredBy:   "manual",
	})
	require.NoError(t, err)
	require.NoError(t, env.Queries.UpdateDeploymentImageTag(ctx, generated.UpdateDeploymentImageTagParams{
		ID:       dep.ID,
		ImageTag: pgtype.Text{String: "my-app:abc12345", Valid: true},
	}))

	env.Runtime.ListImages_ = []runtime.ImageInfo{
		{ID: "imgX", RepoTags: []string{"my-app:abc12345"}, Size: 100}, // owned via deployment
		{ID: "imgY", RepoTags: []string{"redis:7"}, Size: 50},          // unowned
	}
	t.Cleanup(func() { env.Runtime.ListImages_ = nil })

	resp := env.DoRequest(t, "GET", "/api/docker/images", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	images := testutil.ReadJSONArray(t, resp)
	require.Len(t, images, 2)

	owned := images[0].(map[string]any)
	owner, ok := owned["owner"].(map[string]any)
	require.True(t, ok, "platform-built image should be attributed to its app")
	assert.Equal(t, "My App", owner["name"])
	assert.Equal(t, "application", owner["type"])
	assert.Equal(t, proj["id"], owner["project_id"])

	unowned := images[1].(map[string]any)
	assert.Nil(t, unowned["owner"])
}

// TestDockerVolumes_OwnerFromApp verifies application volumes are attributed to
// their owning app by reconstructing the deterministic Docker volume name from
// the application_volumes table (volumes carry no application-id label).
func TestDockerVolumes_OwnerFromApp(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	proj := env.CreateProject(t, adminToken, "Proj", "proj")

	var projID pgtype.UUID
	require.NoError(t, projID.Scan(proj["id"].(string)))

	ctx := context.Background()
	app, err := env.Queries.CreateApplication(ctx, generated.CreateApplicationParams{
		ProjectID: projID,
		Name:      "My App",
		Slug:      "my-app",
		Type:      "git",
		BuildType: "dockerfile",
	})
	require.NoError(t, err)
	_, err = env.Queries.CreateApplicationVolume(ctx, generated.CreateApplicationVolumeParams{
		ApplicationID: app.ID,
		Name:          "data",
		MountPath:     "/data",
	})
	require.NoError(t, err)

	volName := naming.AppVolumeName(uuid.UUID(app.ID.Bytes).String(), "data")
	env.Runtime.ListVolumes_ = []runtime.VolumeInfo{
		{Name: volName, Driver: "local", Size: 100, RefCount: 1}, // owned
		{Name: "some-foreign-vol", Driver: "local", Size: 10},    // unowned
	}
	t.Cleanup(func() { env.Runtime.ListVolumes_ = nil })

	resp := env.DoRequest(t, "GET", "/api/docker/volumes", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	vols := testutil.ReadJSONArray(t, resp)
	require.Len(t, vols, 2)

	owner, ok := vols[0].(map[string]any)["owner"].(map[string]any)
	require.True(t, ok, "app volume should be attributed to its app")
	assert.Equal(t, "My App", owner["name"])
	assert.Equal(t, "application", owner["type"])
	assert.Equal(t, proj["id"], owner["project_id"])

	assert.Nil(t, vols[1].(map[string]any)["owner"])
}

func TestDockerEndpoints_NonAdminForbidden(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "member@test.com", "password123")

	for _, path := range []string{
		"/api/docker/overview",
		"/api/docker/containers",
		"/api/docker/images",
		"/api/docker/volumes",
		"/api/docker/networks",
	} {
		resp := env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(memberToken))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "member should be forbidden from %s", path)
		resp.Body.Close()
	}
}
