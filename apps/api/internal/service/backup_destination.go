package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/service/backup"
	"github.com/belune-hq/belune/internal/store/generated"
)

// BackupDestinationService manages project-scoped S3-compatible backup targets.
// Credentials are stored as a keyring-encrypted JSON blob; the transport client
// is built lazily via the scope-agnostic backup.NewDestinationClient.
type BackupDestinationService struct {
	queries *generated.Queries
	keyring *crypto.Keyring
}

func NewBackupDestinationService(queries *generated.Queries, keyring *crypto.Keyring) *BackupDestinationService {
	return &BackupDestinationService{queries: queries, keyring: keyring}
}

// DestinationCredentials is the secret material for an S3 destination.
type DestinationCredentials struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// SaveBackupDestinationParams holds the fields for creating/updating a
// destination. Credentials is optional on update: nil preserves the stored secret.
type SaveBackupDestinationParams struct {
	ProjectID   pgtype.UUID
	Name        string
	Provider    string
	Endpoint    string
	Region      string
	Bucket      string
	Prefix      string
	UseSSL      bool
	Credentials *DestinationCredentials
}

// Create inserts a new destination, encrypting its credentials. A local
// destination has nothing to encrypt — credentials_encrypted is NOT NULL, so
// it stores an empty (not nil) blob rather than relaxing the column.
func (s *BackupDestinationService) Create(ctx context.Context, p SaveBackupDestinationParams) (generated.BackupDestination, error) {
	var enc []byte
	if p.Provider == "local" {
		enc = []byte{}
	} else {
		if p.Credentials == nil {
			return generated.BackupDestination{}, fmt.Errorf("credentials are required")
		}
		var err error
		if enc, err = s.encryptCredentials(p.Credentials); err != nil {
			return generated.BackupDestination{}, err
		}
	}
	return s.queries.CreateBackupDestination(ctx, generated.CreateBackupDestinationParams{
		ProjectID:            p.ProjectID,
		Name:                 p.Name,
		Provider:             p.Provider,
		Endpoint:             p.Endpoint,
		Region:               p.Region,
		Bucket:               p.Bucket,
		Prefix:               p.Prefix,
		UseSsl:               p.UseSSL,
		CredentialsEncrypted: enc,
	})
}

// ErrDestinationIdentityLocked is returned when an edit would move a
// destination that already holds recorded backups. It carries the count so the
// caller can say how much is at stake.
type ErrDestinationIdentityLocked struct {
	BackupCount int64
}

func (e ErrDestinationIdentityLocked) Error() string {
	return fmt.Sprintf("destination holds %d recorded backup(s); its bucket and endpoint cannot be changed", e.BackupCount)
}

// Update mutates an existing destination. When Credentials is nil the stored
// secret is preserved (COALESCE in the query handles the NULL).
//
// Provider, endpoint and bucket are a destination's identity: recorded backups
// name it to find their objects, so moving it would silently point them at
// storage their data was never written to. Region and credentials stay editable
// — rotation is legitimate and does not move anything.
func (s *BackupDestinationService) Update(ctx context.Context, id pgtype.UUID, p SaveBackupDestinationParams) (generated.BackupDestination, error) {
	current, err := s.queries.GetBackupDestination(ctx, id)
	if err != nil {
		return generated.BackupDestination{}, err
	}
	// An empty endpoint means the client derives one from the region
	// (s3.<region>.amazonaws.com — see backup.newMinioClient), so for those
	// destinations the region IS the address and moving it repoints every
	// recorded backup at storage the bucket does not live behind. Where the
	// endpoint is explicit, region is just a signing hint and stays editable.
	regionIsIdentity := current.Endpoint == "" && p.Region != current.Region

	if p.Provider != current.Provider || p.Endpoint != current.Endpoint ||
		p.Bucket != current.Bucket || regionIsIdentity {
		count, cErr := s.queries.CountLocationsByDestination(ctx, id)
		if cErr != nil {
			return generated.BackupDestination{}, fmt.Errorf("count backups in destination: %w", cErr)
		}
		if count > 0 {
			return generated.BackupDestination{}, ErrDestinationIdentityLocked{BackupCount: count}
		}
	}

	var enc []byte
	if p.Credentials != nil {
		if enc, err = s.encryptCredentials(p.Credentials); err != nil {
			return generated.BackupDestination{}, err
		}
	}
	return s.queries.UpdateBackupDestination(ctx, generated.UpdateBackupDestinationParams{
		ID:                   id,
		Name:                 p.Name,
		Provider:             p.Provider,
		Endpoint:             p.Endpoint,
		Region:               p.Region,
		Bucket:               p.Bucket,
		Prefix:               p.Prefix,
		UseSsl:               p.UseSSL,
		CredentialsEncrypted: enc, // nil preserves existing
	})
}

// ListByProject returns a project's destinations without their secrets.
func (s *BackupDestinationService) ListByProject(ctx context.Context, projectID pgtype.UUID) ([]generated.ListBackupDestinationsByProjectRow, error) {
	return s.queries.ListBackupDestinationsByProject(ctx, projectID)
}

// Get returns the raw destination row (including encrypted credentials).
func (s *BackupDestinationService) Get(ctx context.Context, id pgtype.UUID) (generated.BackupDestination, error) {
	return s.queries.GetBackupDestination(ctx, id)
}

// CountBackupsStored reports how many backups are recorded as living in this
// destination. Used to explain a refused delete.
func (s *BackupDestinationService) CountBackupsStored(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.queries.CountLocationsByDestination(ctx, id)
}

// Delete removes a destination by id.
func (s *BackupDestinationService) Delete(ctx context.Context, id pgtype.UUID) error {
	return s.queries.DeleteBackupDestination(ctx, id)
}

// Resolve loads a destination and decrypts it into a transport-level
// backup.Destination ready for backup.NewDestinationClient. Used by the worker.
func (s *BackupDestinationService) Resolve(ctx context.Context, id pgtype.UUID) (backup.Destination, error) {
	row, err := s.queries.GetBackupDestination(ctx, id)
	if err != nil {
		return backup.Destination{}, err
	}
	return s.toDestination(row)
}

// ClientForDestination resolves the client for a destination id. This is the
// path a backup with a recorded location takes: the destination is read from the
// location row, not re-derived from a config that may since have been repointed.
// Returns a nil client for a local destination, which has no bucket to reach.
func (s *BackupDestinationService) ClientForDestination(ctx context.Context, destinationID pgtype.UUID) (*backup.DestinationClient, error) {
	dest, err := s.Resolve(ctx, destinationID)
	if err != nil {
		return nil, err
	}
	if dest.IsLocal() {
		return nil, nil
	}
	return backup.NewDestinationClient(dest)
}

// ClientForConfig resolves the destination client for a backup config id. Kept
// as the fallback for backups written before locations were recorded, and for
// ad-hoc runs that have no destination row at all.
func (s *BackupDestinationService) ClientForConfig(ctx context.Context, configID pgtype.UUID) (*backup.DestinationClient, error) {
	cfg, err := s.queries.GetDatabaseBackupConfig(ctx, configID)
	if err != nil {
		return nil, err
	}
	dest, err := s.Resolve(ctx, cfg.DestinationID)
	if err != nil {
		return nil, err
	}
	return backup.NewDestinationClient(dest)
}

// ClientForVolumeBackupConfig resolves the destination client for an application
// volume backup config id. Used by the volume restore worker (download) and by
// config deletion (remove the config's remote objects).
func (s *BackupDestinationService) ClientForVolumeBackupConfig(ctx context.Context, configID pgtype.UUID) (*backup.DestinationClient, error) {
	cfg, err := s.queries.GetApplicationVolumeBackupConfig(ctx, configID)
	if err != nil {
		return nil, err
	}
	dest, err := s.Resolve(ctx, cfg.DestinationID)
	if err != nil {
		return nil, err
	}
	return backup.NewDestinationClient(dest)
}

// PurgeVolumeBackupObjects removes the remote objects for the given volume
// backup runs, routing each to the destination it was recorded as written to
// and falling back to fallbackConfigID for runs with no recorded location.
//
// Routing per run matters here: resolving one client from the config's current
// destination sends every key at whatever bucket the config points at now, so
// after a repoint the real objects are silently left behind in the old bucket —
// and their location rows would go on pinning that destination against
// deletion. Location rows for objects actually removed are deleted with them.
//
// Best-effort by design: this runs while tearing a config down, and a storage
// error must not leave the config undeletable. Returns the first error seen so
// the caller can decide, having already done as much as it could.
func (s *BackupDestinationService) PurgeVolumeBackupObjects(ctx context.Context, runs []generated.ApplicationVolumeBackup, fallbackConfigID pgtype.UUID) error {
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	for _, run := range runs {
		locs, err := s.queries.ListLocationsForVolumeBackup(ctx, run.ID)
		if err != nil {
			note(fmt.Errorf("list backup locations: %w", err))
			continue
		}

		if len(locs) == 0 {
			// Written before locations were recorded: the config is the only
			// thing left that knows where it went.
			if !run.RemoteKey.Valid || !fallbackConfigID.Valid {
				continue
			}
			client, cErr := s.ClientForVolumeBackupConfig(ctx, fallbackConfigID)
			if cErr != nil {
				note(cErr)
				continue
			}
			note(client.DeleteFrom(ctx, []string{run.RemoteKey.String}))
			continue
		}

		for _, loc := range locs {
			if loc.RemoteKey.Valid {
				client, cErr := s.ClientForDestination(ctx, loc.DestinationID)
				if cErr != nil {
					note(cErr)
					continue
				}
				if client != nil {
					if dErr := client.DeleteFrom(ctx, []string{loc.RemoteKey.String}); dErr != nil {
						note(dErr)
						continue // keep the row: the object may still be there
					}
				}
			}
			note(s.queries.DeleteBackupLocation(ctx, loc.ID))
		}
	}
	return firstErr
}

// Test builds a client for the stored destination and verifies bucket access.
// A local destination has nothing to reach — it always succeeds.
func (s *BackupDestinationService) Test(ctx context.Context, id pgtype.UUID) error {
	dest, err := s.Resolve(ctx, id)
	if err != nil {
		return err
	}
	if dest.IsLocal() {
		return nil
	}
	client, err := backup.NewDestinationClient(dest)
	if err != nil {
		return err
	}
	return client.Test(ctx)
}

// TestConnection verifies ad-hoc connection params (used by the create/edit form
// before the destination is saved). When credentials are omitted and fallbackID
// is set, the stored destination's credentials are used — so editing without
// re-entering the secret still tests against the form's other (possibly changed)
// fields.
func (s *BackupDestinationService) TestConnection(ctx context.Context, p SaveBackupDestinationParams, fallbackID pgtype.UUID) error {
	if p.Provider == "local" {
		return nil
	}
	accessKey, secretKey := "", ""
	if p.Credentials != nil {
		accessKey, secretKey = p.Credentials.AccessKey, p.Credentials.SecretKey
	}
	if (accessKey == "" || secretKey == "") && fallbackID.Valid {
		if row, err := s.queries.GetBackupDestination(ctx, fallbackID); err == nil {
			if creds, derr := s.decryptCredentials(row.CredentialsEncrypted); derr == nil {
				if accessKey == "" {
					accessKey = creds.AccessKey
				}
				if secretKey == "" {
					secretKey = creds.SecretKey
				}
			}
		}
	}
	if accessKey == "" || secretKey == "" {
		return fmt.Errorf("credentials are required to test the connection")
	}

	client, err := backup.NewDestinationClient(backup.Destination{
		Endpoint:  p.Endpoint,
		Region:    p.Region,
		Bucket:    p.Bucket,
		Prefix:    p.Prefix,
		AccessKey: accessKey,
		SecretKey: secretKey,
		UseSSL:    p.UseSSL,
	})
	if err != nil {
		return err
	}
	return client.Test(ctx)
}

func (s *BackupDestinationService) toDestination(row generated.BackupDestination) (backup.Destination, error) {
	// A local destination has no credentials to decrypt (credentials_encrypted
	// is an empty, not NULL, bytea — see Create/Update) and no client is ever
	// built for it, so skip decryption entirely rather than fail on empty input.
	if row.Provider == "local" {
		return backup.Destination{Provider: row.Provider}, nil
	}
	creds, err := s.decryptCredentials(row.CredentialsEncrypted)
	if err != nil {
		return backup.Destination{}, err
	}
	return backup.Destination{
		Provider:  row.Provider,
		Endpoint:  row.Endpoint,
		Region:    row.Region,
		Bucket:    row.Bucket,
		Prefix:    row.Prefix,
		AccessKey: creds.AccessKey,
		SecretKey: creds.SecretKey,
		UseSSL:    row.UseSsl,
	}, nil
}

func (s *BackupDestinationService) encryptCredentials(c *DestinationCredentials) ([]byte, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal destination credentials: %w", err)
	}
	enc, err := s.keyring.Encrypt(raw)
	if err != nil {
		return nil, fmt.Errorf("encrypt destination credentials: %w", err)
	}
	return enc, nil
}

func (s *BackupDestinationService) decryptCredentials(encrypted []byte) (DestinationCredentials, error) {
	if len(encrypted) == 0 {
		return DestinationCredentials{}, fmt.Errorf("destination has no stored credentials")
	}
	raw, err := s.keyring.Decrypt(encrypted)
	if err != nil {
		return DestinationCredentials{}, fmt.Errorf("decrypt destination credentials: %w", err)
	}
	var creds DestinationCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return DestinationCredentials{}, fmt.Errorf("unmarshal destination credentials: %w", err)
	}
	return creds, nil
}
