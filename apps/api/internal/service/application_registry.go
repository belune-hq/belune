package service

import (
	"fmt"
	"strings"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

// ApplicationRegistryCredentials preserves omitted fields. A host change needs
// a replacement password or explicit removal, so an old token cannot follow an
// image to a different registry. The host itself is derived, never stored twice.
func (s *ApplicationService) ApplicationRegistryCredentials(current generated.Application, kind, image string, username, password *string, clear bool) (pgtype.Text, []byte, error) {
	invalid := func(message string) (pgtype.Text, []byte, error) {
		return pgtype.Text{}, nil, fmt.Errorf("%w: %s", ErrRegistryCredentialInvalid, message)
	}
	if kind != "image" {
		if username != nil || password != nil || clear {
			return invalid("registry credentials apply only to image applications")
		}
		return pgtype.Text{}, nil, nil
	}
	if clear {
		if username != nil || password != nil {
			return invalid("remove the application credential separately from setting a replacement")
		}
		return pgtype.Text{}, nil, nil
	}
	user, sealed := current.RegistryUsername, current.RegistryPasswordEncrypted
	if username != nil {
		user = pgtype.Text{String: strings.TrimSpace(*username), Valid: true}
	}
	if password != nil {
		if *password == "" || len(*password) > 65536 {
			return invalid("registry password must contain 1 to 65536 bytes; omit it to keep the stored password")
		}
		encrypted, err := s.keyring.Encrypt([]byte(*password))
		if err != nil {
			return pgtype.Text{}, nil, fmt.Errorf("encrypt application registry password: %w", err)
		}
		sealed = encrypted
	}
	if len(sealed) == 0 {
		if user.String != "" {
			return invalid("a registry username requires a password")
		}
		return pgtype.Text{}, nil, nil
	}
	if user.String == "" || len(user.String) > 255 {
		return invalid("registry username is required and must be at most 255 characters")
	}
	host, err := runtime.RegistryHost(image)
	if err != nil {
		return invalid("invalid image reference for registry credentials")
	}
	if len(current.RegistryPasswordEncrypted) > 0 && password == nil {
		previous, err := runtime.RegistryHost(current.SourceImage.String)
		if err != nil || previous != host {
			return invalid("changing registry host requires a replacement password or removal of the application credential")
		}
	}
	return user, sealed, nil
}
