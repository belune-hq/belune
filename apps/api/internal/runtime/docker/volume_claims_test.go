package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPruneVolumesHonorsClaimsAndStillRemovesForeignOrphans(t *testing.T) {
	for _, keep := range [][]string{{"compose_pgdata", "legacy-db-vol"}, nil} {
		name := "claimed"
		if len(keep) == 0 {
			name = "empty install"
		}
		t.Run(name, func(t *testing.T) {
			removed := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/volumes") {
					assert.Contains(t, r.URL.Query().Get("filters"), "dangling")
					assert.NoError(t, json.NewEncoder(w).Encode(volume.ListResponse{Volumes: []*volume.Volume{
						{Name: "compose_pgdata"}, {Name: "legacy-db-vol"}, {Name: "foreign-orphan"}, {Name: "belune-legacy"},
						{Name: "labelled", Labels: map[string]string{labelData: "true"}},
					}}))
					return
				}
				if r.Method == http.MethodDelete {
					assert.NotEqual(t, "true", r.URL.Query().Get("force"))
					removed = append(removed, path.Base(r.URL.Path))
					w.WriteHeader(http.StatusNoContent)
					return
				}
				t.Errorf("unexpected Docker call: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			cli, err := dockerclient.NewClientWithOpts(dockerclient.WithHost(server.URL), dockerclient.WithVersion("1.47"))
			require.NoError(t, err)
			defer func() { _ = cli.Close() }()
			c := &Client{cli: cli}
			require.NoError(t, c.PruneVolumes(context.Background(), keep))
			want := []string{"foreign-orphan"}
			if len(keep) == 0 {
				want = append(want, "compose_pgdata", "legacy-db-vol")
			}
			assert.ElementsMatch(t, want, removed)
		})
	}
}

// Slow size enrichment must not hide existing volume metadata from adoption.
func TestListVolumesReturnsMetadataWhenSizeScanStalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/volumes") {
			assert.NoError(t, json.NewEncoder(w).Encode(volume.ListResponse{Volumes: []*volume.Volume{
				{Name: "compose_pgdata", Labels: map[string]string{"source": "compose"}},
			}}))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/system/df") {
			<-r.Context().Done()
			return
		}
		t.Errorf("unexpected Docker call: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.WithHost(server.URL), dockerclient.WithVersion("1.47"))
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	volumes, err := (&Client{cli: cli}).ListVolumes(ctx)
	require.NoError(t, err)
	require.NoError(t, ctx.Err(), "optional scan must stop before the request deadline")
	require.Len(t, volumes, 1)
	assert.Equal(t, "compose_pgdata", volumes[0].Name)
	assert.Equal(t, "compose", volumes[0].Labels["source"])
	assert.Equal(t, int64(-1), volumes[0].Size)
}

func TestVolumeSizesStopsBeforeInventoryDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.WithHost(server.URL), dockerclient.WithVersion("1.47"))
	require.NoError(t, err)
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = (&Client{cli: cli}).VolumeSizes(ctx, []string{"compose_pgdata"})
	require.Error(t, err)
	require.NoError(t, ctx.Err(), "caller must still have time to return volume metadata")
}
