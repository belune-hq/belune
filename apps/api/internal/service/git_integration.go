package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/git/providers"
	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/store/generated"
)

// GitIntegrationService manages per-user connected provider accounts: completing
// the connect flow, listing connections, and minting clone tokens (refreshing
// stored OAuth tokens as needed).
type GitIntegrationService struct {
	queries *generated.Queries
	keyring *crypto.Keyring
	configs *GitProviderConfigService
}

func NewGitIntegrationService(queries *generated.Queries, keyring *crypto.Keyring, configs *GitProviderConfigService) *GitIntegrationService {
	return &GitIntegrationService{queries: queries, keyring: keyring, configs: configs}
}

// appConfig builds a providers.AppConfig from the stored provider app config.
func (s *GitIntegrationService) appConfig(ctx context.Context, provider, baseURL string) (providers.AppConfig, error) {
	cfg, secret, err := s.configs.Get(ctx, provider, baseURL)
	if err != nil {
		return providers.AppConfig{}, fmt.Errorf("provider %q not configured: %w", provider, err)
	}
	return providers.AppConfig{
		Provider:     cfg.Provider,
		BaseURL:      cfg.BaseUrl,
		ClientID:     cfg.ClientID,
		ClientSecret: secret.ClientSecret,
		AppID:        cfg.AppID,
		AppSlug:      cfg.AppSlug,
		PrivateKey:   secret.PrivateKey,
	}, nil
}

// AuthURL returns the provider URL to redirect the user to in order to connect.
func (s *GitIntegrationService) AuthURL(ctx context.Context, provider, baseURL, redirectURI, state string) (string, error) {
	conn, err := providers.For(provider)
	if err != nil {
		return "", err
	}
	cfg, err := s.appConfig(ctx, provider, baseURL)
	if err != nil {
		return "", err
	}
	return conn.AuthURL(cfg, redirectURI, state), nil
}

// Connect completes the provider callback and persists the new connection.
func (s *GitIntegrationService) Connect(ctx context.Context, provider, baseURL, redirectURI string, params url.Values, userID pgtype.UUID) (generated.GitIntegration, error) {
	conn, err := providers.For(provider)
	if err != nil {
		return generated.GitIntegration{}, err
	}
	cfg, err := s.appConfig(ctx, provider, baseURL)
	if err != nil {
		return generated.GitIntegration{}, err
	}
	integ, err := conn.Connect(ctx, cfg, redirectURI, params)
	if err != nil {
		return generated.GitIntegration{}, fmt.Errorf("connect: %w", err)
	}

	encrypted, err := s.encryptIntegration(integ)
	if err != nil {
		return generated.GitIntegration{}, err
	}
	return s.queries.CreateGitIntegration(ctx, generated.CreateGitIntegrationParams{
		Provider:        provider,
		BaseUrl:         baseURL,
		AccountLogin:    integ.AccountLogin,
		ConfigEncrypted: encrypted,
		CreatedBy:       userID,
	})
}

// ListByUser returns a user's connections (without secrets).
func (s *GitIntegrationService) ListByUser(ctx context.Context, userID pgtype.UUID) ([]generated.ListGitIntegrationsByUserRow, error) {
	return s.queries.ListGitIntegrationsByUser(ctx, userID)
}

// Delete removes a connection.
func (s *GitIntegrationService) Delete(ctx context.Context, id pgtype.UUID) error {
	return s.queries.DeleteGitIntegration(ctx, id)
}

// OwnerID returns the user that created a connection, for authorization checks.
func (s *GitIntegrationService) OwnerID(ctx context.Context, id pgtype.UUID) (pgtype.UUID, error) {
	row, err := s.queries.GetGitIntegration(ctx, id)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return row.CreatedBy, nil
}

// resolve loads a connection row, its decrypted Integration state, the provider
// app config, and the connector.
func (s *GitIntegrationService) resolve(ctx context.Context, id pgtype.UUID) (generated.GitIntegration, providers.Integration, providers.AppConfig, providers.Connector, error) {
	row, err := s.queries.GetGitIntegration(ctx, id)
	if err != nil {
		return generated.GitIntegration{}, providers.Integration{}, providers.AppConfig{}, nil, err
	}
	integ, err := s.decryptIntegration(row.ConfigEncrypted)
	if err != nil {
		return generated.GitIntegration{}, providers.Integration{}, providers.AppConfig{}, nil, err
	}
	integ.Provider = row.Provider
	integ.BaseURL = row.BaseUrl
	integ.AccountLogin = row.AccountLogin

	conn, err := providers.For(row.Provider)
	if err != nil {
		return generated.GitIntegration{}, providers.Integration{}, providers.AppConfig{}, nil, err
	}
	cfg, err := s.appConfig(ctx, row.Provider, row.BaseUrl)
	if err != nil {
		return generated.GitIntegration{}, providers.Integration{}, providers.AppConfig{}, nil, err
	}
	return row, integ, cfg, conn, nil
}

// CloneToken mints a clone token for a connection, persisting refreshed OAuth
// tokens. repoURL is the clone target; the token is only minted when repoURL is
// on the provider's expected host, so a malicious source_repo cannot exfiltrate
// the credential to an arbitrary host. Returns the token, the provider name, and
// the account login — all three feed BuildCloneURL, which for some providers
// (gitea, bitbucket) must put a username alongside the token.
func (s *GitIntegrationService) CloneToken(ctx context.Context, id pgtype.UUID, repoURL string) (token, provider, username string, err error) {
	row, integ, cfg, conn, err := s.resolve(ctx, id)
	if err != nil {
		return "", "", "", err
	}
	expected := providerHost(row.Provider, row.BaseUrl)
	got := repoHost(repoURL)
	if expected == "" || got == "" || got != expected {
		return "", "", "", fmt.Errorf("source repo host %q does not match provider host %q", got, expected)
	}
	token, changed, err := conn.CloneToken(ctx, cfg, &integ)
	if err != nil {
		return "", "", "", err
	}
	if changed {
		if encrypted, encErr := s.encryptIntegration(integ); encErr == nil {
			_, _ = s.queries.UpdateGitIntegrationConfig(ctx, generated.UpdateGitIntegrationConfigParams{
				ID:              row.ID,
				ConfigEncrypted: encrypted,
			})
		}
	}
	return token, row.Provider, row.AccountLogin, nil
}

// ListRepos / ListBranches back the application repo/branch pickers.
func (s *GitIntegrationService) ListRepos(ctx context.Context, id pgtype.UUID) ([]providers.Repo, error) {
	_, integ, cfg, conn, err := s.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	return conn.ListRepos(ctx, cfg, &integ)
}

func (s *GitIntegrationService) ListBranches(ctx context.Context, id pgtype.UUID, repoFullName string) ([]providers.Branch, error) {
	_, integ, cfg, conn, err := s.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	return conn.ListBranches(ctx, cfg, &integ, repoFullName)
}

// providerHost returns the expected git host for a provider connection: the
// configured base URL host for self-hosted instances, else the SaaS default.
func providerHost(provider, baseURL string) string {
	if baseURL != "" {
		if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
			return strings.ToLower(u.Host)
		}
	}
	switch provider {
	case "github":
		return "github.com"
	case "gitlab":
		return "gitlab.com"
	case "bitbucket":
		return "bitbucket.org"
	default:
		return "" // gitea is always self-hosted; base URL is required
	}
}

// repoHost extracts the lowercased host from an HTTPS repo URL.
func repoHost(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

func (s *GitIntegrationService) encryptIntegration(integ providers.Integration) ([]byte, error) {
	raw, err := json.Marshal(integ)
	if err != nil {
		return nil, fmt.Errorf("marshal integration: %w", err)
	}
	return s.keyring.Encrypt(raw)
}

func (s *GitIntegrationService) decryptIntegration(encrypted []byte) (providers.Integration, error) {
	raw, err := s.keyring.Decrypt(encrypted)
	if err != nil {
		return providers.Integration{}, fmt.Errorf("decrypt integration: %w", err)
	}
	var integ providers.Integration
	if err := json.Unmarshal(raw, &integ); err != nil {
		return providers.Integration{}, fmt.Errorf("unmarshal integration: %w", err)
	}
	return integ, nil
}
