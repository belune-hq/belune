package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

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
			defer cli.Close()
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
