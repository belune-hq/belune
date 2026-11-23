package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrRegistryCredentialInvalid = errors.New("invalid registry credential")

type RegistryCredentialService struct {
	queries *generated.Queries
	keyring *crypto.Keyring
}

func NewRegistryCredentialService(q *generated.Queries, kr *crypto.Keyring) *RegistryCredentialService {
	return &RegistryCredentialService{queries: q, keyring: kr}
}

// Save preserves every omitted field on update, especially the write-only password.
func (s *RegistryCredentialService) Save(ctx context.Context, id pgtype.UUID, host, username, password *string) (generated.RegistryCredential, error) {
	var current generated.RegistryCredential
	if id.Valid {
		var err error
		current, err = s.queries.GetRegistryCredential(ctx, id)
		if err != nil {
			return current, err
		}
	}
	if host != nil {
		current.RegistryHost = *host
	}
	if username != nil {
		current.Username = strings.TrimSpace(*username)
	}
	normalized, err := runtime.RegistryCredentialHost(current.RegistryHost)
	if err != nil {
		return current, fmt.Errorf("%w: %s", ErrRegistryCredentialInvalid, err)
	}
	current.RegistryHost = normalized
	if current.Username == "" || len(current.Username) > 255 {
		return current, fmt.Errorf("%w: username is required and must be at most 255 characters", ErrRegistryCredentialInvalid)
	}
	var enc []byte
	if password != nil {
		if len(*password) == 0 || len(*password) > 65536 {
			return current, fmt.Errorf("%w: password is required and must be at most 65536 bytes", ErrRegistryCredentialInvalid)
		}
		enc, err = s.keyring.Encrypt([]byte(*password))
		if err != nil {
			return current, fmt.Errorf("encrypt registry password: %w", err)
		}
	}
	if !id.Valid {
		if enc == nil {
			return current, fmt.Errorf("%w: password is required", ErrRegistryCredentialInvalid)
		}
		return s.queries.CreateRegistryCredential(ctx, generated.CreateRegistryCredentialParams{RegistryHost: current.RegistryHost, Username: current.Username, PasswordEncrypted: enc})
	}
	return s.queries.UpdateRegistryCredential(ctx, generated.UpdateRegistryCredentialParams{ID: id, RegistryHost: current.RegistryHost, Username: current.Username, PasswordEncrypted: enc})
}

// Resolve chooses application, then matching install-wide, then anonymous auth.
// The app credential wins because its owner knows which account can pull the image.
// Derive its host from source_image; a second stored host could drift and leak a token.
func (s *RegistryCredentialService) Resolve(ctx context.Context, applicationID pgtype.UUID, image string) (*runtime.RegistryAuth, error) {
	host, err := runtime.RegistryHost(image)
	if err != nil {
		return nil, err
	}
	if applicationID.Valid {
		app, err := s.queries.GetApplication(ctx, applicationID)
		if err != nil {
			return nil, fmt.Errorf("resolve application registry credential: %w", err)
		}
		if len(app.RegistryPasswordEncrypted) > 0 {
			appHost, err := runtime.RegistryHost(app.SourceImage.String)
			if err != nil || appHost != host {
				return nil, fmt.Errorf("application registry credential does not match the image host")
			}
			password, err := s.keyring.Decrypt(app.RegistryPasswordEncrypted)
			if err != nil {
				return nil, fmt.Errorf("decrypt application registry credential: %w", err)
			}
			return &runtime.RegistryAuth{Username: app.RegistryUsername.String, Password: string(password), ServerAddress: host}, nil
		}
	}
	row, err := s.queries.GetRegistryCredentialByHost(ctx, host)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve registry credential for %s: %w", host, err)
	}
	password, err := s.keyring.Decrypt(row.PasswordEncrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt registry credential for %s: %w", host, err)
	}
	return &runtime.RegistryAuth{Username: row.Username, Password: string(password), ServerAddress: host}, nil
}

// Source exposes only which tier would be selected, never an install-wide login.
func (s *RegistryCredentialService) Source(ctx context.Context, app generated.Application) (string, error) {
	if len(app.RegistryPasswordEncrypted) > 0 {
		return "application", nil
	}
	if app.Type != "image" {
		return "anonymous", nil
	}
	host, err := runtime.RegistryHost(app.SourceImage.String)
	if err != nil {
		return "anonymous", nil
	}
	exists, err := s.queries.RegistryCredentialExistsByHost(ctx, host)
	if err != nil {
		return "", err
	}
	if exists {
		return "install_wide", nil
	}
	return "anonymous", nil
}
