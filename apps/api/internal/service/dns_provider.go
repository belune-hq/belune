package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/store/generated"
)

// DNSProviderService manages the DNS credentials behind the ACME DNS-01
// challenge. Credentials are one keyring-encrypted JSON blob per provider, the
// same shape as backup destinations.
type DNSProviderService struct {
	queries *generated.Queries
	keyring *crypto.Keyring
}

func NewDNSProviderService(queries *generated.Queries, keyring *crypto.Keyring) *DNSProviderService {
	return &DNSProviderService{queries: queries, keyring: keyring}
}

// ErrDNSProviderInUse is returned when a delete would strand domains that rely
// on the provider. It carries the count so the caller can say how many.
type ErrDNSProviderInUse struct {
	DomainCount int64
}

func (e ErrDNSProviderInUse) Error() string {
	return fmt.Sprintf("%d domain(s) use this DNS provider", e.DomainCount)
}

// Create stores a provider after validating its credentials against the
// provider's spec.
func (s *DNSProviderService) Create(ctx context.Context, name, provider string, fields map[string]string) (generated.DnsProvider, error) {
	spec, ok := proxy.DNSProviders[provider]
	if !ok {
		return generated.DnsProvider{}, fmt.Errorf("unsupported DNS provider %q", provider)
	}
	clean, err := spec.Normalize(fields)
	if err != nil {
		return generated.DnsProvider{}, err
	}
	enc, err := s.encrypt(clean)
	if err != nil {
		return generated.DnsProvider{}, err
	}
	return s.queries.CreateDNSProvider(ctx, generated.CreateDNSProviderParams{
		Name:                 strings.TrimSpace(name),
		Provider:             provider,
		CredentialsEncrypted: enc,
	})
}

// Update renames a provider and, when fields is non-nil, replaces its
// credentials wholesale. Wholesale rather than merged: a rotation that supplied
// only the new api_token must not keep a stale zone_token beside it. nil fields
// keeps the stored secret.
func (s *DNSProviderService) Update(ctx context.Context, id pgtype.UUID, name string, fields map[string]string) (generated.DnsProvider, error) {
	current, err := s.queries.GetDNSProvider(ctx, id)
	if err != nil {
		return generated.DnsProvider{}, err
	}
	var enc []byte
	if fields != nil {
		clean, err := proxy.DNSProviders[current.Provider].Normalize(fields)
		if err != nil {
			return generated.DnsProvider{}, err
		}
		if enc, err = s.encrypt(clean); err != nil {
			return generated.DnsProvider{}, err
		}
	}
	return s.queries.UpdateDNSProvider(ctx, generated.UpdateDNSProviderParams{
		ID:                   id,
		Name:                 strings.TrimSpace(name),
		CredentialsEncrypted: enc, // nil preserves the stored secret
	})
}

func (s *DNSProviderService) Get(ctx context.Context, id pgtype.UUID) (generated.DnsProvider, error) {
	return s.queries.GetDNSProvider(ctx, id)
}

// List returns providers without their secrets.
func (s *DNSProviderService) List(ctx context.Context) ([]generated.ListDNSProvidersRow, error) {
	return s.queries.ListDNSProviders(ctx)
}

// Delete removes a provider, refusing while a domain still points at it.
func (s *DNSProviderService) Delete(ctx context.Context, id pgtype.UUID) error {
	n, err := s.queries.CountDomainsByDNSProvider(ctx, id)
	if err != nil {
		return fmt.Errorf("counting domains for dns provider: %w", err)
	}
	if n > 0 {
		return ErrDNSProviderInUse{DomainCount: n}
	}
	return s.queries.DeleteDNSProvider(ctx, id)
}

func (s *DNSProviderService) encrypt(fields map[string]string) ([]byte, error) {
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("marshalling dns credentials: %w", err)
	}
	enc, err := s.keyring.Encrypt(raw)
	if err != nil {
		return nil, fmt.Errorf("encrypting dns credentials: %w", err)
	}
	return enc, nil
}
