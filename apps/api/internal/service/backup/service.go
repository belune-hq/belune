// Package backup provides S3-compatible object storage for off-host backups.
// The service wraps github.com/minio/minio-go/v7 so a single client covers
// AWS S3, Cloudflare R2, Backblaze B2, Wasabi, and self-hosted MinIO.
package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/belune-hq/belune/internal/config"
)

// BackupObject is a single remote backup entry returned by List.
type BackupObject struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Service uploads and manages backup archives in an S3-compatible bucket. It
// is safe for concurrent use. Remote config (LoadRemoteConfig) is re-resolved
// on every call — cheap, and it's what lets a dashboard-edited Remote Storage
// card take effect on the very next backup without an API restart. The minio
// client is rebuilt only when the resolved config actually changed since the
// last call, not on every call.
type Service struct {
	cfg *config.Config

	mu     sync.Mutex
	client *minio.Client // nil until the first call that needs one
	built  RemoteConfig  // the config the cached client was built from
}

// New creates a backup service. The client is not dialled until the first
// operation; NewService never returns an error.
func New(cfg *config.Config) *Service {
	return &Service{cfg: cfg}
}

// Enabled reports whether remote backup is configured.
func (s *Service) Enabled() bool {
	rc := LoadRemoteConfig(s.cfg)
	return rc.Enabled && rc.Bucket != ""
}

// Upload uploads localPath to the configured bucket and returns the remote key.
// The remote key is derived from the filename: <prefix><basename>.
func (s *Service) Upload(ctx context.Context, localPath string) (string, error) {
	rc, err := s.resolveClient()
	if err != nil {
		return "", err
	}

	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open backup file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat backup file: %w", err)
	}

	key := rc.Prefix + filepath.Base(localPath)
	contentType := "application/gzip"
	if strings.HasSuffix(localPath, ".age") {
		contentType = "application/octet-stream"
	}

	_, err = s.client.PutObject(ctx, rc.Bucket, key, f, info.Size(),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return "", fmt.Errorf("upload to s3: %w", err)
	}
	return key, nil
}

// Download fetches the object at key into localPath, creating parent dirs as
// needed. Used to restore a managed-database backup from S3.
func (s *Service) Download(ctx context.Context, key, localPath string) error {
	rc, err := s.resolveClient()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("create download dir: %w", err)
	}
	if err := s.client.FGetObject(ctx, rc.Bucket, key, localPath,
		minio.GetObjectOptions{}); err != nil {
		return fmt.Errorf("download from s3: %w", err)
	}
	return nil
}

// List returns all backup objects under the configured prefix, sorted oldest-first.
func (s *Service) List(ctx context.Context) ([]BackupObject, error) {
	rc, err := s.resolveClient()
	if err != nil {
		return nil, err
	}

	var objects []BackupObject
	for obj := range s.client.ListObjects(ctx, rc.Bucket,
		minio.ListObjectsOptions{Prefix: rc.Prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list objects: %w", obj.Err)
		}
		objects = append(objects, BackupObject{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		})
	}

	sort.Slice(objects, func(i, j int) bool {
		return objects[i].LastModified.Before(objects[j].LastModified)
	})
	return objects, nil
}

// Delete removes the given keys from the bucket. Missing keys are silently ignored.
func (s *Service) Delete(ctx context.Context, keys []string) error {
	rc, err := s.resolveClient()
	if err != nil {
		return err
	}

	objectsCh := make(chan minio.ObjectInfo, len(keys))
	for _, key := range keys {
		objectsCh <- minio.ObjectInfo{Key: key}
	}
	close(objectsCh)

	var errs []string
	for result := range s.client.RemoveObjects(ctx, rc.Bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		if result.Err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", result.ObjectName, result.Err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("delete objects: %s", strings.Join(errs, "; "))
	}
	return nil
}

// LatestKey returns the key and upload timestamp of the most-recent backup,
// or empty string and zero time when no backups exist.
func (s *Service) LatestKey(ctx context.Context) (string, time.Time, error) {
	objects, err := s.List(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	if len(objects) == 0 {
		return "", time.Time{}, nil
	}
	latest := objects[len(objects)-1]
	return latest.Key, latest.LastModified, nil
}

// Check verifies the remote storage is reachable and the configured bucket
// exists, without mutating anything. Used by the admin "Test connection"
// action. Returns an error describing the failure (unreachable, bad
// credentials, or missing bucket).
func (s *Service) Check(ctx context.Context) error {
	rc, err := s.resolveClient()
	if err != nil {
		return err
	}
	exists, err := s.client.BucketExists(ctx, rc.Bucket)
	if err != nil {
		return fmt.Errorf("reach bucket: %w", err)
	}
	if !exists {
		return fmt.Errorf("bucket %q does not exist", rc.Bucket)
	}
	return nil
}

// EnsureBucket creates the configured bucket if it does not already exist.
// This is used in tests and can be called during initial provisioning.
func (s *Service) EnsureBucket(ctx context.Context) error {
	rc, err := s.resolveClient()
	if err != nil {
		return err
	}
	exists, err := s.client.BucketExists(ctx, rc.Bucket)
	if err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, rc.Bucket,
		minio.MakeBucketOptions{Region: rc.Region}); err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}
	return nil
}

// SelectForDeletion returns the keys that should be deleted under the given
// retention policy. objects must be sorted oldest-first (as returned by List).
// The retainCount newest objects are always kept regardless of age. Beyond
// that, objects whose LastModified is older than retainDays days are deleted.
func SelectForDeletion(objects []BackupObject, now time.Time, retainDays, retainCount int) []string {
	if len(objects) == 0 {
		return nil
	}

	// keepFrom is the index of the first object in the "always keep" tail.
	keepFrom := max(len(objects)-retainCount, 0)

	cutoff := now.AddDate(0, 0, -retainDays)

	var toDelete []string
	for i, obj := range objects {
		if i >= keepFrom {
			break
		}
		if obj.LastModified.Before(cutoff) {
			toDelete = append(toDelete, obj.Key)
		}
	}
	return toDelete
}

// Rotate applies the retention policy: deletes remote objects that are beyond
// retainCount AND older than retainDays. Returns the deleted keys. The caller
// resolves the retention values fresh (dashboard settings, falling back to
// Config.BackupRetainDays/Count) — Service stays DB-agnostic, same reasoning
// as it not resolving RemoteConfig's DB-backed pieces itself.
func (s *Service) Rotate(ctx context.Context, retainDays, retainCount int) ([]string, error) {
	objects, err := s.List(ctx)
	if err != nil {
		return nil, err
	}

	keys := SelectForDeletion(objects, time.Now(), retainDays, retainCount)
	if len(keys) == 0 {
		return nil, nil
	}

	if err := s.Delete(ctx, keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// resolveClient resolves the current remote config and returns a ready
// s.client, rebuilding it only when the resolved config changed since the
// last call (RemoteConfig is a flat comparable struct, so this is a cheap
// ==) — every public method starts with `rc, err := s.resolveClient()`.
func (s *Service) resolveClient() (RemoteConfig, error) {
	rc := LoadRemoteConfig(s.cfg)
	if !rc.Enabled || rc.Bucket == "" {
		return rc, fmt.Errorf("remote backup is not enabled (BACKUP_REMOTE_ENABLED=false or bucket empty)")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil && s.built == rc {
		return rc, nil
	}
	client, err := newMinioClient(rc.Endpoint, rc.Region, rc.AccessKey, rc.SecretKey, rc.UseSSL)
	if err != nil {
		return rc, fmt.Errorf("create s3 client: %w", err)
	}
	s.client = client
	s.built = rc
	return rc, nil
}
