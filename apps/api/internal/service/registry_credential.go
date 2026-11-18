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

// Resolve decrypts immediately before an application pull. Missing credentials
// are valid for public images; the runtime explains anonymous auth failures.
func (s *RegistryCredentialService) Resolve(ctx context.Context, image string) (*runtime.RegistryAuth, error) {
	host, err := runtime.RegistryHost(image)
	if err != nil {
		return nil, err
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
