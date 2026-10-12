package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/weiliang79/belune/internal/runtime"
	"github.com/weiliang79/belune/internal/service/backup"
	"github.com/weiliang79/belune/internal/status"
	"github.com/weiliang79/belune/internal/store"
	"github.com/weiliang79/belune/internal/store/generated"
)

type DatabaseService struct {
	db           *pgxpool.Pool
	queries      *generated.Queries
	runtimes     runtime.Runtimes
	backups      *backup.Service           // optional; nil disables global remote backup cleanup
	destinations *BackupDestinationService // optional; routes config-backup remote cleanup
}

func NewDatabaseService(db *pgxpool.Pool, queries *generated.Queries, rts runtime.Runtimes, backups *backup.Service, destinations *BackupDestinationService) *DatabaseService {
	return &DatabaseService{db: db, queries: queries, runtimes: rts, backups: backups, destinations: destinations}
}

// deleteRemoteBackup removes a backup's remote object. It routes to the
// destination the backup was recorded as written to, falling back to the
// config's current destination (backups predating 000061) and then the global
// target. Best-effort — deleting from the wrong bucket would leave the real
// object behind, which is why the recorded location wins.
func (s *DatabaseService) deleteRemoteBackup(ctx context.Context, b generated.DatabaseBackup) {
	if !b.RemoteKey.Valid {
		return
	}
	if s.destinations != nil {
		locs, err := s.queries.ListLocationsForDatabaseBackup(ctx, b.ID)
		if err != nil {
			slog.Warn("could not list backup locations for remote cleanup", "key", b.RemoteKey.String, "error", err)
		}
		for _, loc := range locs {
			if !loc.RemoteKey.Valid {
				continue
			}
			client, cerr := s.destinations.ClientForDestination(ctx, loc.DestinationID)
			if cerr != nil {
				slog.Warn("could not resolve recorded destination for remote backup cleanup", "key", loc.RemoteKey.String, "error", cerr)
				continue
			}
			if client == nil {
				continue // local destination: the file is handled by the caller
			}
			if derr := client.DeleteFrom(ctx, []string{loc.RemoteKey.String}); derr != nil {
				slog.Warn("could not remove remote backup", "key", loc.RemoteKey.String, "error", derr)
			}
		}
		if len(locs) > 0 {
			return
		}
	}
	if b.BackupConfigID.Valid && s.destinations != nil {
		client, err := s.destinations.ClientForConfig(ctx, b.BackupConfigID)
		if err != nil {
			slog.Warn("could not resolve destination for remote backup cleanup", "key", b.RemoteKey.String, "error", err)
			return
		}
		if err := client.DeleteFrom(ctx, []string{b.RemoteKey.String}); err != nil {
			slog.Warn("could not remove remote backup", "key", b.RemoteKey.String, "error", err)
		}
		return
	}
	if s.backups != nil && s.backups.Enabled() {
		if err := s.backups.Delete(ctx, []string{b.RemoteKey.String}); err != nil {
			slog.Warn("could not remove remote backup", "key", b.RemoteKey.String, "error", err)
		}
	}
}

// DeleteBackup removes a single backup belonging to dbID: its local file, S3
// object, and row. Returns an error if the backup isn't found or doesn't belong
// to the database.
func (s *DatabaseService) DeleteBackup(ctx context.Context, dbID, backupID pgtype.UUID) error {
	b, err := s.queries.GetDatabaseBackup(ctx, backupID)
	if err != nil {
		return err
	}
	if b.DatabaseID != dbID {
		return fmt.Errorf("backup does not belong to this database")
	}
	if b.LocalPath.Valid {
		if err := os.Remove(b.LocalPath.String); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove backup file", "path", b.LocalPath.String, "error", err)
		}
	}
	s.deleteRemoteBackup(ctx, b)
	return s.queries.DeleteDatabaseBackup(ctx, backupID)
}

// DeleteOrphanedBackup erases a backup whose database is gone: the archive, any
// remote copy, and the row. Without it the inventory could only show what an
// install is paying for, never stop it — the object outlives every screen that
// used to reach it.
func (s *DatabaseService) DeleteOrphanedBackup(ctx context.Context, backupID pgtype.UUID) error {
	b, err := s.queries.GetDatabaseBackup(ctx, backupID)
	if err != nil {
		return err
	}
	if !b.TombstoneID.Valid {
		return fmt.Errorf("backup still belongs to a live database")
	}
	if b.LocalPath.Valid {
		if err := os.Remove(b.LocalPath.String); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove orphaned backup file", "path", b.LocalPath.String, "error", err)
		}
	}
	s.deleteRemoteBackup(ctx, b)
	if err := s.queries.DeleteDatabaseBackup(ctx, backupID); err != nil {
		return err
	}
	// A tombstone exists to give backups a parent. Once its last one is gone it
	// describes nothing, so it goes too rather than accumulating forever.
	remaining, err := s.queries.CountBackupsForTombstone(ctx, b.TombstoneID)
	if err != nil {
		slog.Warn("could not count remaining backups for tombstone", "error", err)
		return nil
	}
	if remaining == 0 {
		if err := s.queries.DeleteDatabaseTombstone(ctx, b.TombstoneID); err != nil {
			slog.Warn("could not remove an empty tombstone", "error", err)
		}
	}
	return nil
}

// cleanupBackupsPageSize bounds one listing pass; cleanupBackups keeps going
// until a short page. The old single capped call was silently partial: the
// delete-confirmation dialog counts every backup, so a database with more than
// one page promised destruction that never happened and left the surplus S3
// objects with no row pointing at them — unreachable, unprunable, still billed.
const cleanupBackupsPageSize = 1000

// cleanupBackups removes a database's backup archives (local files and, when
// configured, S3 objects). Best-effort: the DB-row delete cascades regardless.
func (s *DatabaseService) cleanupBackups(ctx context.Context, dbID pgtype.UUID) {
	for {
		rows, err := s.queries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{
			DatabaseID: dbID, Limit: cleanupBackupsPageSize,
		})
		if err != nil {
			slog.Warn("could not list backups during db deletion", "error", err)
			return
		}
		if len(rows) == 0 {
			return
		}
		// Bail when a page removes nothing: the listing has no offset, so a page
		// that cannot delete its rows would be handed back identically forever.
		if s.cleanupBackupPage(ctx, rows) == 0 {
			slog.Warn("stopping backup cleanup: no rows could be removed",
				"remaining", len(rows))
			return
		}
		if len(rows) < cleanupBackupsPageSize {
			return
		}
	}
}

// cleanupBackupPage erases one page of backups: local file, remote object, and
// row. The row delete is what lets the caller page — the listing has no offset,
// so each pass must shrink the set it is walking. Returns how many rows went.
func (s *DatabaseService) cleanupBackupPage(ctx context.Context, rows []generated.DatabaseBackup) int {
	removed := 0
	for _, b := range rows {
		if b.LocalPath.Valid {
			if err := os.Remove(b.LocalPath.String); err != nil && !os.IsNotExist(err) {
				slog.Warn("could not remove backup file during db deletion", "path", b.LocalPath.String, "error", err)
			}
		}
		// Route remote cleanup per backup (config backups live in project
		// destinations; ad-hoc runs in the global target).
		s.deleteRemoteBackup(ctx, b)
		if err := s.queries.DeleteDatabaseBackup(ctx, b.ID); err != nil {
			slog.Warn("could not delete backup row during db deletion",
				"backup_id", b.ID, "error", err)
			continue
		}
		removed++
	}
	return removed
}

// DeletionImpact is what deleting a database takes with it beyond the database
// itself. database_backups.database_id is ON DELETE CASCADE and cleanupBackups
// erases the remote objects too, so this is destruction the operator has to be
// shown before they consent to it — not a warning after the fact.
type DeletionImpact struct {
	BackupCount  int64
	Destinations []string
}

// DeletionImpact counts the backups a delete would destroy and names the
// destinations holding copies. Destination names come from recorded locations,
// so backups written before 000061 are counted but their bucket is not named.
func (s *DatabaseService) DeletionImpact(ctx context.Context, dbID pgtype.UUID) (DeletionImpact, error) {
	count, err := s.queries.CountDatabaseBackupsWithArtifacts(ctx, dbID)
	if err != nil {
		return DeletionImpact{}, fmt.Errorf("count database backups: %w", err)
	}
	names, err := s.queries.ListDestinationNamesForDatabaseBackups(ctx, dbID)
	if err != nil {
		return DeletionImpact{}, fmt.Errorf("list backup destinations: %w", err)
	}
	return DeletionImpact{BackupCount: count, Destinations: names}, nil
}

// Delete stops and removes the database container and its volume, then deletes
// the DB record.
//
// keepBackups decides what happens to the backups. Keeping them writes a
// tombstone and re-points them onto it, so they remain listable and restorable
// after the database is gone; destroying them erases the archives and the rows.
// The caller states the choice explicitly — there is no default here, because
// the two outcomes differ by whether yesterday's data still exists.
func (s *DatabaseService) Delete(ctx context.Context, dbID pgtype.UUID, keepBackups bool) error {
	db, err := s.queries.GetDatabase(ctx, dbID)
	if err != nil {
		return err
	}

	// Resolved while the row is still there — the lookup joins projects.
	rt, err := RuntimeForDatabase(ctx, s.queries, s.runtimes, dbID)
	if err != nil {
		return err
	}

	if err := rt.StopContainer(ctx, db.Slug); err != nil {
		slog.Warn("could not stop container during db deletion", "container", db.Slug, "error", err)
	}
	if err := rt.RemoveContainer(ctx, db.Slug); err != nil {
		slog.Warn("could not remove container during db deletion", "container", db.Slug, "error", err)
	}
	if err := rt.RemoveVolume(ctx, db.Slug+"-vol"); err != nil {
		slog.Warn("could not remove volume during db deletion", "volume", db.Slug+"-vol", "error", err)
	}

	if keepBackups {
		// A tombstone exists to give backups a parent. With none to keep it
		// would describe nothing while holding a second copy of the database's
		// credentials, waiting on a sweep an operator is allowed to disable.
		count, cErr := s.queries.CountDatabaseBackups(ctx, dbID)
		if cErr != nil {
			return fmt.Errorf("counting backups before deletion: %w", cErr)
		}
		if count > 0 {
			return s.keepBackupsAndDelete(ctx, db)
		}
	}

	s.cleanupBackups(ctx, dbID)

	// The rows go deterministically even if an object could not be erased above.
	// Object cleanup is best-effort by design, but a leftover row still pointing
	// at this database would trip the one_parent CHECK the moment the delete
	// nulls its database_id, and abort the whole thing.
	return store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		if err := q.DeleteDatabaseBackupsForDatabase(ctx, dbID); err != nil {
			return fmt.Errorf("removing backup rows: %w", err)
		}
		return q.DeleteDatabase(ctx, dbID)
	})
}

// keepBackupsAndDelete records what the database was and moves its backups onto
// that record, then deletes the database.
//
// All three steps share a transaction. Half of this is worse than none of it: a
// tombstone with no backups is a row describing something nobody can restore,
// and backups whose re-point succeeded while the delete failed would be
// detached from a database that is still running.
func (s *DatabaseService) keepBackupsAndDelete(ctx context.Context, db generated.Database) error {
	return store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		tombstone, err := q.CreateDatabaseTombstone(ctx, generated.CreateDatabaseTombstoneParams{
			ProjectID:  db.ProjectID,
			OriginalID: db.ID,
			Slug:       db.Slug,
			Name:       db.Name,
			Type:       db.Type,
			Version:    pgtype.Text{String: db.Version, Valid: true},
			// Carried across as ciphertext: the tombstone column is a rewrap
			// target like the one it came from, so this is never decrypted here.
			CredentialsEncrypted: db.CredentialsEncrypted,
			// What provisioning needs that the engine name does not imply.
			Image:          db.Image,
			ContainerPort:  db.ContainerPort,
			DataDir:        db.DataDir,
			BackupMode:     pgtype.Text{String: db.BackupMode, Valid: true},
			BackupCommand:  db.BackupCommand,
			RestoreCommand: db.RestoreCommand,
			CpuLimit:       pgtype.Float8{Float64: db.CpuLimit, Valid: true},
			MemoryLimit:    pgtype.Int8{Int64: db.MemoryLimit, Valid: true},
			ImageDigest:    db.ImageDigest,
		})
		if err != nil {
			return fmt.Errorf("recording the deleted database: %w", err)
		}
		if err := q.ReparentDatabaseBackupsToTombstone(ctx, generated.ReparentDatabaseBackupsToTombstoneParams{
			DatabaseID:  db.ID,
			TombstoneID: tombstone.ID,
		}); err != nil {
			return fmt.Errorf("moving backups onto the tombstone: %w", err)
		}
		return q.DeleteDatabase(ctx, db.ID)
	})
}

// RestoreFromTombstone recreates a deleted database from its tombstone and
// hands its surviving backups back to it.
//
// It comes back under the ORIGINAL slug and credentials. That is the whole
// point rather than a nicety: the slug is the container name, so it is the
// hostname every dependent application resolves, and attaching a database
// injects no connection env vars. A restore into a differently-named database
// leaves every application in the project pointing at a host that is not there,
// which is why "restore to a new database" is not offered as the safe option.
//
// The row is created here rather than in the worker so the caller gets it back
// immediately and the UI can show it provisioning. Bringing the container up
// and applying the archive is the worker's job.
func (s *DatabaseService) RestoreFromTombstone(ctx context.Context, tombstoneID, backupID pgtype.UUID) (generated.Database, error) {
	tombstone, err := s.queries.GetDatabaseTombstone(ctx, tombstoneID)
	if err != nil {
		return generated.Database{}, fmt.Errorf("get tombstone: %w", err)
	}

	// The slug is taken verbatim and needs no collision check. Creation builds
	// it as {projectSlug}-{baseSlug}-{first 8 hex of the database's own id}, so
	// it is unique by construction and nothing else can occupy it — which also
	// means the replacement's slug carries the ORIGINAL database's id fragment
	// rather than its own. That mismatch is the point: the slug is a hostname
	// applications already resolve, not a description of the row.

	var created generated.Database
	err = store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		created, err = q.CreateDatabase(ctx, generated.CreateDatabaseParams{
			ProjectID: tombstone.ProjectID,
			Type:      tombstone.Type,
			Name:      tombstone.Name,
			Slug:      tombstone.Slug,
			Version:   tombstone.Version.String,
			Status:    status.DatabaseCreating,
			// Left for provisioning to stamp, exactly as a new database does.
			InternalHost: pgtype.Text{},
			InternalPort: pgtype.Int4{},
			// Carried as ciphertext; never decrypted on this path.
			CredentialsEncrypted: tombstone.CredentialsEncrypted,
			Image:                tombstone.Image,
			ContainerPort:        tombstone.ContainerPort,
			DataDir:              tombstone.DataDir,
			BackupMode:           tombstone.BackupMode.String,
			BackupCommand:        tombstone.BackupCommand,
			RestoreCommand:       tombstone.RestoreCommand,
		})
		if err != nil {
			return fmt.Errorf("recreating the database: %w", err)
		}

		// Limits and the pinned digest are separate columns from creation's
		// point of view, but not from the replacement's: a database that comes
		// back uncapped is not the same database — on a small host the cap is
		// what keeps the box alive — and an empty digest makes provisioning
		// re-resolve a mutable tag, so it could come back on a different image
		// than the dump was taken from.
		if tombstone.CpuLimit.Valid || tombstone.MemoryLimit.Valid {
			if created, err = q.UpdateDatabaseResources(ctx, generated.UpdateDatabaseResourcesParams{
				ID:          created.ID,
				CpuLimit:    tombstone.CpuLimit.Float64,
				MemoryLimit: tombstone.MemoryLimit.Int64,
			}); err != nil {
				return fmt.Errorf("restoring resource limits: %w", err)
			}
		}
		if tombstone.ImageDigest.Valid {
			if err := q.UpdateDatabaseImageDigest(ctx, generated.UpdateDatabaseImageDigestParams{
				ID:          created.ID,
				ImageDigest: tombstone.ImageDigest,
			}); err != nil {
				return fmt.Errorf("restoring the image pin: %w", err)
			}
		}

		// The backups move back onto the live database in the same transaction
		// that creates it. Leaving them on the tombstone would mean the
		// replacement starts with no history and the tombstone lingers
		// describing a database that exists again.
		if err := q.ReclaimBackupsFromTombstone(ctx, generated.ReclaimBackupsFromTombstoneParams{
			TombstoneID: tombstoneID,
			DatabaseID:  created.ID,
		}); err != nil {
			return fmt.Errorf("returning backups to the database: %w", err)
		}
		return q.DeleteDatabaseTombstone(ctx, tombstoneID)
	})
	if err != nil {
		return generated.Database{}, err
	}
	return created, nil
}

// ErrNegativeLimit is returned when a caller supplies a negative CPU or memory
// limit. Zero is legitimate and means "unlimited", so only a negative value is
// an error.
var ErrNegativeLimit = errors.New("cpu_limit and memory_limit must be non-negative")

// SetResources persists a managed database's CPU and memory limits and applies
// them to the running container.
//
// Shared by the REST endpoint and the MCP tool so the live-apply cannot drift
// between them — it is the half that is easy to omit, and omitting it looks
// like nothing happened until the container is next recreated.
//
// ⚠️ A live-apply failure is deliberately NOT fatal: the limit is already
// persisted and will be honoured the next time the container is created, so
// failing the call would report "nothing happened" when something did. It is
// logged instead.
//
// cpu is cores (0 = unlimited), memory is bytes (0 = unlimited). The bool
// reports whether the live apply actually SUCCEEDED, so a caller can say what
// happened instead of assuming — the MCP tool's reply does exactly that, and
// without this it claimed the limits were in force whenever the database
// happened to be running.
func (s *DatabaseService) SetResources(ctx context.Context, dbID pgtype.UUID, cpu float64, memory int64) (generated.Database, bool, error) {
	if cpu < 0 || memory < 0 {
		return generated.Database{}, false, ErrNegativeLimit
	}

	db, err := s.queries.UpdateDatabaseResources(ctx, generated.UpdateDatabaseResourcesParams{
		ID:          dbID,
		CpuLimit:    cpu,
		MemoryLimit: memory,
	})
	if err != nil {
		return generated.Database{}, false, fmt.Errorf("persist database resources: %w", err)
	}

	appliedLive := false
	if db.Status == status.DatabaseRunning && db.InternalHost.Valid {
		rt, err := RuntimeForDatabase(ctx, s.queries, s.runtimes, dbID)
		if err != nil {
			slog.Warn("failed to resolve server to apply database resource limits live",
				"database_id", uuidString(dbID), "error", err)
		} else if err := rt.UpdateContainerResources(ctx, db.InternalHost.String, cpu, memory); err != nil {
			slog.Warn("failed to apply database resource limits live",
				"database_id", uuidString(dbID), "error", err)
		} else {
			appliedLive = true
		}
	}

	return db, appliedLive, nil
}

// NewDatabase is the transport-independent shape for provisioning a managed
// database, so a caller outside internal/handler can ask for one without that
// package's json-tagged request type.
//
// ⚠️ Deliberately narrower than the REST request. It carries no "other"-engine
// fields (image, container_port, data_dir, env, backup commands): those
// describe an arbitrary container run as a database, which is a configuration
// a person assembles from an image's documentation, not something to be
// assembled from a conversation. A caller needing one uses the dashboard.
type NewDatabase struct {
	Name    string
	Type    string // postgres | mysql | redis | mongo
	Version string // empty = the engine's default
}
