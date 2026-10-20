package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/runtime"
)

// labelCache is a per-volume flag that opts the volume out of PruneVolumes.
// Used by CNB/BuildKit cache volumes so layer history survives cleanup runs.
const labelCache = "belune-cache"

// labelData is a per-volume flag that opts a persistent application data
// volume out of PruneVolumes. Unlike caches (disposable), these hold user data
// and must NEVER be reaped by the cleanup worker, even while their app's
// container is absent between deploys (which would otherwise leave the volume
// dangling and eligible for prune).
const labelData = "belune-data"

func (c *Client) CreateVolume(ctx context.Context, name string) error {
	_, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name: name,
		Labels: map[string]string{
			labelManagedBy: labelValue,
		},
	})
	if err != nil {
		return fmt.Errorf("create volume %s: %w", name, err)
	}
	return nil
}

func (c *Client) CreateCacheVolume(ctx context.Context, name string) error {
	_, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name: name,
		Labels: map[string]string{
			labelManagedBy: labelValue,
			labelCache:     "true",
		},
	})
	if err != nil {
		return fmt.Errorf("create cache volume %s: %w", name, err)
	}
	return nil
}

// CreateDataVolume creates (or no-ops on) a Docker volume labelled as
// persistent application data. The data label opts the volume out of
// PruneVolumes so the cleanup worker cannot delete user data when the app's
// container is absent between deploys. Idempotent: VolumeCreate returns the
// existing volume if the name already exists.
func (c *Client) CreateDataVolume(ctx context.Context, name string) error {
	_, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name: name,
		Labels: map[string]string{
			labelManagedBy: labelValue,
			labelData:      "true",
		},
	})
	if err != nil {
		return fmt.Errorf("create data volume %s: %w", name, err)
	}
	return nil
}

// VolumeSize returns the on-disk usage of a named volume. Uses DiskUsage
// rather than VolumeInspect because size is only populated in the disk-usage
// API (inspect returns -1 unless the daemon was called with ?size=1, which
// the Docker Go SDK does not expose).
func (c *Client) VolumeSize(ctx context.Context, name string) (int64, error) {
	sizes, err := c.VolumeSizes(ctx, []string{name})
	if err != nil {
		return 0, err
	}
	return sizes[name], nil
}

// VolumeSizes runs a single DiskUsage call and extracts sizes for each named
// volume. DiskUsage is O(all volumes on the host); calling it once per
// lookup in a loop quickly exceeds the request timeout on busy hosts.
func (c *Client) VolumeSizes(ctx context.Context, names []string) (map[string]int64, error) {
	out := make(map[string]int64, len(names))
	if len(names) == 0 {
		return out, nil
	}
	du, err := c.cli.DiskUsage(ctx, types.DiskUsageOptions{
		Types: []types.DiskUsageObject{types.VolumeObject},
	})
	if err != nil {
		return nil, fmt.Errorf("disk usage: %w", err)
	}
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	for _, v := range du.Volumes {
		if v == nil || v.UsageData == nil {
			continue
		}
		if _, ok := want[v.Name]; ok {
			out[v.Name] = v.UsageData.Size
		}
	}
	return out, nil
}

func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.cli.VolumeRemove(ctx, name, true)
}

// ListVolumes lists all volumes on the host for the read-only admin inspect
// page, attaching on-disk sizes and reference counts from a single DiskUsage
// call (DiskUsage is O(all volumes), so it must not be called per-volume).
func (c *Client) ListVolumes(ctx context.Context) (result []runtime.VolumeInfo, err error) {
	defer func() { metrics.RecordDockerOp("list_volumes", err) }()

	list, err := c.cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}

	// Sizes/ref-counts only come from the disk-usage endpoint. Best-effort: if
	// it fails we still return the volume list with -1 (unknown) sizes.
	usage := make(map[string]*volume.UsageData)
	if du, duErr := c.cli.DiskUsage(ctx, types.DiskUsageOptions{
		Types: []types.DiskUsageObject{types.VolumeObject},
	}); duErr == nil {
		for _, v := range du.Volumes {
			if v != nil && v.UsageData != nil {
				usage[v.Name] = v.UsageData
			}
		}
	}

	result = make([]runtime.VolumeInfo, 0, len(list.Volumes))
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		size, refCount := int64(-1), int64(-1)
		if ud, ok := usage[v.Name]; ok {
			size, refCount = ud.Size, ud.RefCount
		}
		created, _ := time.Parse(time.RFC3339, v.CreatedAt)
		result = append(result, runtime.VolumeInfo{
			Name:       v.Name,
			Driver:     v.Driver,
			Mountpoint: v.Mountpoint,
			Scope:      v.Scope,
			Size:       size,
			RefCount:   refCount,
			Labels:     v.Labels,
			CreatedAt:  created,
		})
	}
	return result, nil
}

// PruneBuildCache reclaims build caches: it removes the platform's CNB cache
// volumes (labelled belune-cache — deliberately preserved by PruneVolumes) and
// prunes the BuildKit builder cache. Both are disposable; the next build
// repopulates them. Best-effort: individual failures are logged, not fatal.
func (c *Client) PruneBuildCache(ctx context.Context) (err error) {
	defer func() { metrics.RecordDockerOp("prune_build_cache", err) }()

	// Remove CNB cache volumes (label belune-cache=true).
	list, listErr := c.cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", labelCache+"=true")),
	})
	if listErr != nil {
		return fmt.Errorf("list cache volumes: %w", listErr)
	}
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		if rmErr := c.cli.VolumeRemove(ctx, v.Name, false); rmErr != nil {
			slog.Warn("prune build cache: could not remove cache volume", "volume", v.Name, "error", rmErr)
		}
	}

	// Prune the BuildKit builder cache.
	if _, bcErr := c.cli.BuildCachePrune(ctx, build.CachePruneOptions{All: true}); bcErr != nil {
		return fmt.Errorf("prune builder cache: %w", bcErr)
	}
	return nil
}

// platformVolumePrefix marks Docker volumes created by the platform's naming
// helpers (app data volumes `belune-vol-*`, CNB/BuildKit caches `belune-cnb-*`,
// and other `belune-*` resources). Kept in sync with internal/naming.
const platformVolumePrefix = "belune-"

// isPlatformVolume reports whether a volume was created by the platform and must
// therefore never be reaped by PruneVolumes. A volume qualifies if it carries
// any platform label OR matches the platform naming prefix. The name check is a
// deliberate safety net: Docker's VolumeCreate does not (re)apply labels to a
// volume that already exists, so a legacy or Docker-auto-created data volume can
// end up unlabeled yet still hold user data. Deleting such a volume would be
// silent, unrecoverable data loss, so we err on the side of keeping it — the
// platform removes its own volumes explicitly at app/database deletion.
func isPlatformVolume(name string, labels map[string]string) bool {
	if labels[labelManagedBy] == labelValue ||
		labels[labelCache] == "true" ||
		labels[labelData] == "true" {
		return true
	}
	return strings.HasPrefix(name, platformVolumePrefix)
}

// PruneVolumes reclaims dangling (unreferenced) volumes that are NOT owned by
// the platform. It deliberately does not use the daemon's blanket VolumesPrune:
// that reaps every dangling volume lacking a specific label, which would destroy
// a persistent app or database data volume that is momentarily unreferenced (app
// stopped, database reconfiguring, or a container removed between deploys) and
// happens to be unlabeled. Instead it lists dangling volumes and skips every
// platform-owned one (see isPlatformVolume), removing only foreign orphans.
// Removal is non-forced, so a volume that becomes referenced between the list
// and the remove is left untouched.
func (c *Client) PruneVolumes(ctx context.Context) (err error) {
	defer func() { metrics.RecordDockerOp("prune_volumes", err) }()

	resp, err := c.cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("dangling", "true")),
	})
	if err != nil {
		return fmt.Errorf("list dangling volumes: %w", err)
	}
	for _, v := range resp.Volumes {
		if v == nil || isPlatformVolume(v.Name, v.Labels) {
			continue
		}
		if rmErr := c.cli.VolumeRemove(ctx, v.Name, false); rmErr != nil {
			// Best-effort: the volume may have become referenced since listing,
			// or removal may be denied — never fail the whole cleanup run.
			slog.Warn("prune volumes: could not remove foreign volume", "volume", v.Name, "error", rmErr)
		}
	}
	return nil
}
