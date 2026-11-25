package worker_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/hibiken/asynq"
	mobycontainer "github.com/moby/moby/api/types/container"
	mobynetwork "github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/belune-hq/belune/internal/runtime"
	dockerruntime "github.com/belune-hq/belune/internal/runtime/docker"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// The prune is host-wide. This drill MUST NOT point at the developer daemon:
// only an isolated, marked Docker-in-Docker daemon ever receives destructive calls.
func TestAdoptedVolumeDockerLifecycle(t *testing.T) {
	if os.Getenv("BELUNE_ADOPTION_INTEGRATION") != "1" {
		t.Skip("set BELUNE_ADOPTION_INTEGRATION=1 for isolated Docker drill")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	outer, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	require.NoError(t, err)
	defer func() { _ = outer.Close() }()
	outerInfo, err := outer.Info(ctx)
	require.NoError(t, err)
	marker := fmt.Sprintf("belune-adoption-drill=%d", time.Now().UnixNano())
	daemon, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "docker:27.5.1-dind", ExposedPorts: []string{"2375/tcp"}, Env: map[string]string{"DOCKER_TLS_CERTDIR": ""},
			Cmd: []string{"dockerd", "--host=tcp://0.0.0.0:2375", "--tls=false", "--storage-driver=vfs", "--label=" + marker},
			HostConfigModifier: func(c *mobycontainer.HostConfig) {
				c.Privileged = true
				c.PortBindings = mobynetwork.PortMap{mobynetwork.MustParsePort("2375/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1")}}}
				c.Tmpfs = map[string]string{"/var/lib/docker": "rw,exec,size=256m"}
			},
			WaitingFor: wait.ForHTTP("/_ping").WithPort("2375/tcp").WithStartupTimeout(90 * time.Second),
		}, Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, daemon.Terminate(context.Background())) })
	host, err := daemon.Host(ctx)
	require.NoError(t, err)
	port, err := daemon.MappedPort(ctx, "2375/tcp")
	require.NoError(t, err)
	endpoint := "tcp://" + net.JoinHostPort(host, port.Port())
	require.NotEqual(t, os.Getenv("DOCKER_HOST"), endpoint)
	t.Setenv("DOCKER_HOST", endpoint)
	inner, err := dockerclient.NewClientWithOpts(dockerclient.WithHost(endpoint), dockerclient.WithAPIVersionNegotiation())
	require.NoError(t, err)
	defer func() { _ = inner.Close() }()
	assertIsolation := func(t *testing.T) {
		require.Equal(t, endpoint, os.Getenv("DOCKER_HOST"), "refusing destructive drill on a different endpoint")
		info, err := inner.Info(ctx)
		require.NoError(t, err)
		require.NotEqual(t, outerInfo.ID, info.ID, "refusing to prune the developer daemon")
		require.Contains(t, info.Labels, marker, "daemon must be the marked disposable instance")
	}
	assertIsolation(t)
	// Transfer an existing tiny image; only image reads/pulls touch the outer daemon.
	if _, err := outer.ImageInspect(ctx, "busybox:1.37.0"); err != nil {
		pull, err := outer.ImagePull(ctx, "busybox:1.37.0", image.PullOptions{})
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, pull)
		_ = pull.Close()
		require.NoError(t, err)
	}
	archive, err := outer.ImageSave(ctx, []string{"busybox:1.37.0"})
	require.NoError(t, err)
	loaded, err := inner.ImageLoad(ctx, archive)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, loaded.Body)
	_ = loaded.Body.Close()
	_ = archive.Close()
	require.NoError(t, err)
	rt, err := dockerruntime.New()
	require.NoError(t, err)
	defer func() { _ = rt.Close() }()
	require.NoError(t, rt.CreateNetwork(ctx, "belune-infra"))
	readData := func(t *testing.T, name string) {
		_, err := inner.VolumeInspect(ctx, name)
		require.NoError(t, err, "adopted volume must survive deletion and manual cleanup")
		var out, stderr bytes.Buffer
		code, err := rt.RunHelper(ctx, runtime.ContainerConfig{Image: "busybox:1.37.0", Cmd: []string{"/bin/cat", "/data/marker"}, Volumes: map[string]string{name: "/data"}}, nil, &out, &stderr)
		require.NoError(t, err)
		require.Zero(t, code, stderr.String())
		require.Equal(t, "existing-adopted-data\n", out.String())
	}
	createData := func(t *testing.T, name string) {
		_, err := inner.VolumeCreate(ctx, volume.CreateOptions{Name: name})
		require.NoError(t, err)
		var out, stderr bytes.Buffer
		code, err := rt.RunHelper(ctx, runtime.ContainerConfig{Image: "busybox:1.37.0", Cmd: []string{"/bin/sh", "-c", "printf 'existing-adopted-data\\n' > /data/marker"}, Volumes: map[string]string{name: "/data"}}, nil, &out, &stderr)
		require.NoError(t, err)
		require.Zero(t, code, stderr.String())
	}
	for _, scenario := range []string{"stop", "delete"} {
		t.Run(scenario, func(t *testing.T) {
			assertIsolation(t)
			require.NoError(t, testutil.TruncateAll(ctx, testPool))
			t.Cleanup(func() { require.NoError(t, testutil.TruncateAll(context.Background(), testPool)) })
			app, dep := seedApp(t, func(p *generated.CreateApplicationParams, _ *generated.CreateDeploymentParams) {
				p.SourceImage.String = "busybox:1.37.0"
				p.SourceImage.Valid = true
			})
			_, err := testPool.Exec(ctx, `UPDATE application_process_types SET command=$2 WHERE application_id=$1`, app.ID, "trap 'exit 0' TERM; cat /data/marker; while :; do sleep 1; done")
			require.NoError(t, err)
			h := newTestHandler(rt, &testutil.MockProxyManager{})
			svc := service.NewApplicationService(testPool, testQueries, h.Runtimes, h.Keyring, t.TempDir(), nil)
			name := fmt.Sprintf("compose-%s-%d", scenario, time.Now().UnixNano())
			foreign := name + "-foreign"
			createData(t, name)
			createData(t, foreign)
			params := generated.CreateApplicationVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data"}
			require.NoError(t, params.ExternalName.Scan(name))
			_, err = svc.AdoptVolume(ctx, params)
			require.NoError(t, err)
			inspected, err := inner.VolumeInspect(ctx, name)
			require.NoError(t, err)
			require.Empty(t, inspected.Labels, "adoption must not infer protection from relabelling")
			require.NoError(t, h.HandleDeployTask(ctx, asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID))))
			var appOutput string
			require.Eventually(t, func() bool {
				stream, err := inner.ContainerLogs(ctx, app.Slug, container.LogsOptions{ShowStdout: true, ShowStderr: true})
				if err != nil {
					return false
				}
				defer func() { _ = stream.Close() }()
				var out, stderr bytes.Buffer
				_, err = stdcopy.StdCopy(&out, &stderr, stream)
				appOutput = out.String()
				return err == nil && strings.Contains(appOutput, "existing-adopted-data")
			}, 10*time.Second, 100*time.Millisecond, "application must read the pre-existing data")
			t.Logf("application read from adopted volume: %q", appOutput)
			if scenario == "delete" {
				row, err := testQueries.GetApplicationWithProjectSlug(ctx, app.ID)
				require.NoError(t, err)
				require.NoError(t, svc.Delete(ctx, app.ID, row.ProjectSlug, app.Slug))
				_, err = inner.VolumeInspect(ctx, name)
				require.NoError(t, err, "adopted data must survive application deletion before cleanup")
			} else {
				// Stopping alone still references the volume. Remove the stopped container
				// to reproduce the dangling interval between deploys and prove the mutant.
				require.NoError(t, rt.StopContainer(ctx, app.Slug))
				require.NoError(t, rt.RemoveContainer(ctx, app.Slug))
			}
			assertIsolation(t)
			dangling, err := inner.VolumeList(ctx, volume.ListOptions{Filters: filters.NewArgs(filters.Arg("dangling", "true"))})
			require.NoError(t, err)
			names := []string{}
			for _, v := range dangling.Volumes {
				names = append(names, v.Name)
			}
			require.Contains(t, names, name, "negative control requires an actually dangling adopted volume")
			_, err = testPool.Exec(ctx, `INSERT INTO settings(key,value) VALUES('daily_cleanup_enabled','false') ON CONFLICT(key) DO UPDATE SET value='false'`)
			require.NoError(t, err)
			require.NoError(t, h.HandleCleanupTask(ctx, asynq.NewTask("cleanup", []byte(`{"actions":["volumes"],"scheduled":false}`))))
			readData(t, name)
			_, err = inner.VolumeInspect(ctx, foreign)
			require.True(t, errdefs.IsNotFound(err), "unadopted foreign volume must still be pruned")
		})
	}
	// Legacy managed DB data can be unlabelled and lack the belune- prefix.
	require.NoError(t, testutil.TruncateAll(ctx, testPool))
	db := seedDatabase(t)
	createData(t, db.Slug+"-vol")
	h := newTestHandler(rt, &testutil.MockProxyManager{})
	assertIsolation(t)
	require.NoError(t, h.HandleCleanupTask(ctx, asynq.NewTask("cleanup", []byte(`{"actions":["volumes"],"scheduled":false}`))))
	readData(t, db.Slug+"-vol")
}
