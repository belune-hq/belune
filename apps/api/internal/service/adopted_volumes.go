package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
)

var (
	ErrExternalVolumeName     = errors.New("external_name must be a literal Docker volume name, at most 255 characters")
	ErrExternalVolumeManaged  = errors.New("this volume belongs to Belune; choose an external volume")
	ErrExternalVolumeMissing  = errors.New("the external Docker volume does not exist")
	ErrExternalVolumeClaimed  = errors.New("this volume is already attached; one volume can belong to only one application")
	ErrExternalVolumeInUse    = errors.New("this volume is used by an external container; remove that container before adopting it")
	ErrExternalVolumeLocal    = errors.New("adopting volumes on remote servers is not supported")
	ErrExternalVolumeConflict = errors.New("a volume with this name or mount path already exists on this application")
)
var ErrExternalVolumeConsumer = errors.New("this volume is still mounted by another application or helper; reload or remove its old container before adopting it")

var externalVolumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// AdoptVolume never creates or relabels a volume. The lock must precede the
// existence check, or a prune with an older snapshot can remove it afterwards.
func (s *ApplicationService) AdoptVolume(ctx context.Context, params generated.CreateApplicationVolumeParams) (generated.ApplicationVolume, error) {
	var result generated.ApplicationVolume
	name := params.ExternalName.String
	if !params.ExternalName.Valid || len(name) > 255 || !externalVolumeName.MatchString(name) {
		return result, ErrExternalVolumeName
	}
	if strings.HasPrefix(name, "belune-") {
		return result, ErrExternalVolumeManaged
	}
	err := store.WithVolumeClaimsLock(ctx, s.db, func(ctx context.Context, q *generated.Queries) error {
		app, err := q.GetApplicationWithProjectSlug(ctx, params.ApplicationID)
		if err != nil {
			return fmt.Errorf("get application for adoption: %w", err)
		}
		local, err := q.GetLocalServer(ctx)
		if err != nil {
			return fmt.Errorf("get local server: %w", err)
		}
		if app.ServerID != local.ID {
			return ErrExternalVolumeLocal
		}
		if _, err = q.GetExternalVolumeAttachment(ctx, params.ExternalName); err == nil {
			return ErrExternalVolumeClaimed
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read volume attachment: %w", err)
		}
		databases, err := q.ListAllDatabases(ctx)
		if err != nil {
			return fmt.Errorf("list database volume owners: %w", err)
		}
		for _, db := range databases {
			if db.Slug+"-vol" == name {
				return ErrExternalVolumeManaged
			}
		}
		rt, err := s.runtimes.Local(ctx)
		if err != nil {
			return fmt.Errorf("resolve adoption host: %w", err)
		}
		volumes, err := rt.ListVolumes(ctx)
		if err != nil {
			return fmt.Errorf("check external volume: %w", err)
		}
		found := false
		for _, v := range volumes {
			if v.Name == name {
				found = true
				if v.Labels["managed-by"] == "belune" || v.Labels["belune-data"] == "true" || v.Labels["belune-cache"] == "true" || v.Labels["belune-system"] != "" {
					return ErrExternalVolumeManaged
				}
			}
		}
		if !found {
			return ErrExternalVolumeMissing
		}
		containers, err := rt.ListAllContainers(ctx)
		if err != nil {
			return fmt.Errorf("check volume consumers: %w", err)
		}
		for _, c := range containers {
			for _, v := range c.VolumeNames {
				if v != name {
					continue
				}
				if c.Labels["managed-by"] != "belune" {
					return ErrExternalVolumeInUse
				}
				// Detaching a DB row does not remove a running container's mount. Only
				// this application's own retained mount can be reattached immediately.
				if c.Labels["application-id"] != uuidToString(params.ApplicationID) {
					return ErrExternalVolumeConsumer
				}
			}
		}
		if err := q.RegisterAdoptedVolume(ctx, name); err != nil {
			return fmt.Errorf("register adopted volume: %w", err)
		}
		result, err = q.CreateApplicationVolume(ctx, params)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrExternalVolumeConflict
		}
		return err
	})
	return result, err
}
