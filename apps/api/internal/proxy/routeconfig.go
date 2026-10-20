package proxy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/jackc/pgx/v5/pgtype"
)

// ResolveUpstreamPort picks the container port a domain's traffic is proxied to:
// the domain's own override first, then the application's configured
// container_port, then the 8080 default. Keeps routing in step with
// resolveContainerPort (the deploy health-check path), so a blank per-domain port
// follows the app's port instead of always falling straight to 8080.
func ResolveUpstreamPort(domainPort, appPort pgtype.Int4) int32 {
	if domainPort.Valid && domainPort.Int32 > 0 {
		return domainPort.Int32
	}
	if appPort.Valid && appPort.Int32 > 0 {
		return appPort.Int32
	}
	return 8080
}

// BuildRouteConfig constructs a RouteConfig from a domain and its pre-loaded
// features. healthCheckPath is the application-level health endpoint; pass "" to
// disable Caddy upstream health probing for this route. cert carries the already
// decrypted PEM pair for ssl_mode=custom, and is the zero value otherwise.
func BuildRouteConfig(domain generated.Domain, appContainerPort pgtype.Int4, containerName, healthCheckPath string, features []generated.DomainRouteFeature, cert HostCertificate) RouteConfig {
	port := ResolveUpstreamPort(domain.ContainerPort, appContainerPort)

	var routeFeatures []RouteFeature
	for _, f := range features {
		routeFeatures = append(routeFeatures, RouteFeature{
			Type:    f.FeatureType,
			Config:  json.RawMessage(f.Config),
			Enabled: f.Enabled,
		})
	}

	return RouteConfig{
		Hostname:        domain.Hostname,
		Path:            domain.Path,
		StripPath:       domain.StripPath,
		InternalPath:    domain.InternalPath,
		TargetURL:       fmt.Sprintf("http://%s:%d", containerName, port),
		TLS:             domain.SslEnabled,
		ForceHTTPS:      domain.ForceHttps,
		SSLMode:         domain.SslMode,
		SSLProvider:     domain.SslProvider.String,
		CertPEM:         cert.CertPEM,
		KeyPEM:          cert.KeyPEM,
		Features:        routeFeatures,
		AdvancedConfig:  domain.AdvancedConfig,
		HealthCheckPath: healthCheckPath,
	}
}

// BuildRouteConfigFromDB loads a domain's features and, when it serves an
// uploaded certificate, decrypts that certificate's PEM pair for in-band delivery
// to the proxy.
func BuildRouteConfigFromDB(ctx context.Context, queries *generated.Queries, dec Decryptor, domain generated.Domain, containerName, healthCheckPath string) (RouteConfig, error) {
	dbFeatures, err := queries.ListRouteFeaturesByDomain(ctx, domain.ID)
	if err != nil {
		return RouteConfig{}, fmt.Errorf("list route features for %s: %w", domain.Hostname, err)
	}

	app, err := queries.GetApplication(ctx, domain.ApplicationID)
	if err != nil {
		return RouteConfig{}, fmt.Errorf("load application for %s: %w", domain.Hostname, err)
	}

	cert, err := ResolveCertificate(ctx, queries, dec, domain)
	if err != nil {
		return RouteConfig{}, err
	}

	return BuildRouteConfig(domain, app.ContainerPort, containerName, healthCheckPath, dbFeatures, cert), nil
}

// ResolveCertificate returns the decrypted PEM pair a domain serves, or the zero
// value when it has none (any ssl_mode other than custom, or custom with no
// certificate selected yet — the latter surfaces as a TLS error at route setup
// rather than failing the whole deploy here).
func ResolveCertificate(ctx context.Context, queries *generated.Queries, dec Decryptor, domain generated.Domain) (HostCertificate, error) {
	if domain.SslMode != SSLModeCustom || !domain.CertificateID.Valid {
		return HostCertificate{}, nil
	}

	row, err := queries.GetCertificate(ctx, domain.CertificateID)
	if err != nil {
		return HostCertificate{}, fmt.Errorf("load certificate for %s: %w", domain.Hostname, err)
	}

	certPEM, err := dec.Decrypt(row.CertPemEncrypted)
	if err != nil {
		return HostCertificate{}, fmt.Errorf("decrypt certificate for %s: %w", domain.Hostname, err)
	}
	keyPEM, err := dec.Decrypt(row.KeyPemEncrypted)
	if err != nil {
		return HostCertificate{}, fmt.Errorf("decrypt certificate key for %s: %w", domain.Hostname, err)
	}

	return HostCertificate{
		Hostname: domain.Hostname,
		CertPEM:  string(certPEM),
		KeyPEM:   string(keyPEM),
	}, nil
}
