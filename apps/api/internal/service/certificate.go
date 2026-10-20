package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/store/generated"
)

// ErrCertificateInUse is returned when a certificate cannot be deleted because
// domains still serve it. The FK is ON DELETE RESTRICT, so the database refuses
// regardless; this turns that into an error the handler can explain.
var ErrCertificateInUse = errors.New("certificate is in use by one or more domains")

// CertificateService manages the centralised certificate store: operators upload
// a PEM pair once and reference it from any number of domains. Both PEM blocks
// are envelope-encrypted with the keyring, and the private key is never returned
// to a caller — only the metadata parsed from the leaf at upload time.
type CertificateService struct {
	queries *generated.Queries
	keyring *crypto.Keyring
}

func NewCertificateService(queries *generated.Queries, keyring *crypto.Keyring) *CertificateService {
	return &CertificateService{queries: queries, keyring: keyring}
}

// CertificateMetadata is the safe projection of a stored certificate: everything
// the UI needs, and nothing that could leak key material.
type CertificateMetadata struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Issuer      string     `json:"issuer"`
	Subjects    []string   `json:"subjects"`
	NotBefore   *time.Time `json:"not_before"`
	NotAfter    *time.Time `json:"not_after"`
	DomainCount int64      `json:"domain_count"`
	CreatedAt   time.Time  `json:"created_at"`
}

// parsedCertificate is the metadata extracted from an uploaded leaf.
type parsedCertificate struct {
	issuer    string
	subjects  []string
	notBefore time.Time
	notAfter  time.Time
}

// parseCertificatePair validates an uploaded PEM pair and extracts the leaf's
// metadata. It rejects anything Caddy would later choke on — unparseable PEM, a
// key that does not match the certificate — at upload time, where the operator
// is present to see the error, rather than at request time behind a failed
// handshake.
func parseCertificatePair(certPEM, keyPEM string) (parsedCertificate, error) {
	certPEM = strings.TrimSpace(certPEM)
	keyPEM = strings.TrimSpace(keyPEM)
	if certPEM == "" || keyPEM == "" {
		return parsedCertificate{}, fmt.Errorf("certificate and key are both required")
	}

	// X509KeyPair parses the chain and proves the private key matches the leaf.
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return parsedCertificate{}, fmt.Errorf("certificate and key do not form a valid pair: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return parsedCertificate{}, fmt.Errorf("no certificate found in PEM input")
	}

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return parsedCertificate{}, fmt.Errorf("parse certificate: %w", err)
	}

	// SANs are what Caddy matches on for SNI; a certificate with none can never
	// be selected for a hostname, so treat that as an upload error rather than
	// storing something that silently never serves.
	subjects := append([]string(nil), leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		subjects = append(subjects, ip.String())
	}
	if len(subjects) == 0 {
		return parsedCertificate{}, fmt.Errorf("certificate has no subject alternative names")
	}

	return parsedCertificate{
		issuer:    leaf.Issuer.String(),
		subjects:  subjects,
		notBefore: leaf.NotBefore,
		notAfter:  leaf.NotAfter,
	}, nil
}

// looksLikePEM reports whether s contains at least one PEM block, so obviously
// wrong input (a DER file, a path, an empty paste) fails with a clear message
// instead of a cryptic parse error.
func looksLikePEM(s string) bool {
	block, _ := pem.Decode([]byte(s))
	return block != nil
}

// CreateCertificate validates, encrypts and stores an uploaded PEM pair.
func (s *CertificateService) CreateCertificate(ctx context.Context, name, certPEM, keyPEM string, createdBy pgtype.UUID) (CertificateMetadata, error) {
	if strings.TrimSpace(name) == "" {
		return CertificateMetadata{}, fmt.Errorf("name is required")
	}
	if !looksLikePEM(certPEM) {
		return CertificateMetadata{}, fmt.Errorf("certificate must be PEM-encoded (expected a -----BEGIN CERTIFICATE----- block)")
	}
	if !looksLikePEM(keyPEM) {
		return CertificateMetadata{}, fmt.Errorf("private key must be PEM-encoded (expected a -----BEGIN ... KEY----- block)")
	}

	parsed, err := parseCertificatePair(certPEM, keyPEM)
	if err != nil {
		return CertificateMetadata{}, err
	}

	certEnc, err := s.keyring.Encrypt([]byte(strings.TrimSpace(certPEM)))
	if err != nil {
		return CertificateMetadata{}, fmt.Errorf("encrypt certificate: %w", err)
	}
	keyEnc, err := s.keyring.Encrypt([]byte(strings.TrimSpace(keyPEM)))
	if err != nil {
		return CertificateMetadata{}, fmt.Errorf("encrypt certificate key: %w", err)
	}

	row, err := s.queries.CreateCertificate(ctx, generated.CreateCertificateParams{
		Name:             strings.TrimSpace(name),
		CertPemEncrypted: certEnc,
		KeyPemEncrypted:  keyEnc,
		Issuer:           pgtype.Text{String: parsed.issuer, Valid: parsed.issuer != ""},
		Subjects:         parsed.subjects,
		NotBefore:        pgtype.Timestamptz{Time: parsed.notBefore, Valid: true},
		NotAfter:         pgtype.Timestamptz{Time: parsed.notAfter, Valid: true},
		CreatedBy:        createdBy,
	})
	if err != nil {
		return CertificateMetadata{}, fmt.Errorf("store certificate: %w", err)
	}

	return CertificateMetadata{
		ID:        uuidString(row.ID),
		Name:      row.Name,
		Issuer:    row.Issuer.String,
		Subjects:  row.Subjects,
		NotBefore: timePtr(row.NotBefore),
		NotAfter:  timePtr(row.NotAfter),
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

// UsableCertificate is the reduced shape a non-admin may see: enough to pick a
// certificate and judge whether it is current, and nothing more. It is
// deliberately NOT CertificateMetadata — that carries Subjects, every SAN the
// certificate was issued for, which would tell a member the hostnames of other
// people's applications.
type UsableCertificate struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Issuer   string     `json:"issuer"`
	NotAfter *time.Time `json:"not_after"`
}

// certificateHostnameCandidates turns a hostname into the subject strings that
// would cover it: the name itself, plus its one-label wildcard parent.
//
// RFC 6125 wildcards match exactly one label and only in the leftmost position,
// so "*.example.com" covers "app.example.com" but NOT "a.b.example.com" (two
// labels) and NOT "example.com" (zero). Generating the candidates and comparing
// for equality keeps that rule here, in one place with a test, rather than in a
// SQL LIKE pattern where "*" would have to be translated and the label count
// silently stops being enforced.
//
// Comparison is case-insensitive and ignores a trailing root dot, because DNS
// is — subjects come from the certificate's own DNSNames, whose case is
// whatever the issuer emitted.
func certificateHostnameCandidates(hostname string) []string {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	if h == "" {
		return nil
	}
	candidates := []string{h}
	// A single-label host ("localhost") has no parent to wildcard.
	if i := strings.Index(h, "."); i > 0 {
		candidates = append(candidates, "*"+h[i:])
	}
	return candidates
}

// CertificatesForHostname returns the certificates that can serve one hostname.
// Empty (never an error) for a hostname that resolves to no candidates, so a
// caller can offer an empty picker rather than special-casing blank input.
func (s *CertificateService) CertificatesForHostname(ctx context.Context, hostname string) ([]UsableCertificate, error) {
	candidates := certificateHostnameCandidates(hostname)
	if len(candidates) == 0 {
		return []UsableCertificate{}, nil
	}
	rows, err := s.queries.ListCertificatesForHostname(ctx, candidates)
	if err != nil {
		return nil, fmt.Errorf("list certificates for hostname: %w", err)
	}
	out := make([]UsableCertificate, 0, len(rows))
	for _, row := range rows {
		out = append(out, UsableCertificate{
			ID:       uuidString(row.ID),
			Name:     row.Name,
			Issuer:   row.Issuer.String,
			NotAfter: timePtr(row.NotAfter),
		})
	}
	return out, nil
}

// ListCertificates returns metadata for every stored certificate, with the
// number of domains referencing each. Key material is never included.
func (s *CertificateService) ListCertificates(ctx context.Context) ([]CertificateMetadata, error) {
	rows, err := s.queries.ListCertificates(ctx)
	if err != nil {
		return nil, fmt.Errorf("list certificates: %w", err)
	}

	out := make([]CertificateMetadata, 0, len(rows))
	for _, row := range rows {
		out = append(out, CertificateMetadata{
			ID:          uuidString(row.ID),
			Name:        row.Name,
			Issuer:      row.Issuer.String,
			Subjects:    row.Subjects,
			NotBefore:   timePtr(row.NotBefore),
			NotAfter:    timePtr(row.NotAfter),
			DomainCount: row.DomainCount,
			CreatedAt:   row.CreatedAt.Time,
		})
	}
	return out, nil
}

// DeleteCertificate removes a certificate. Domains still serving it block the
// delete: removing it would break TLS for them with no warning.
func (s *CertificateService) DeleteCertificate(ctx context.Context, id pgtype.UUID) error {
	hostnames, err := s.queries.ListDomainsByCertificate(ctx, id)
	if err != nil {
		return fmt.Errorf("check certificate usage: %w", err)
	}
	if len(hostnames) > 0 {
		return fmt.Errorf("%w: %s", ErrCertificateInUse, strings.Join(hostnames, ", "))
	}

	if err := s.queries.DeleteCertificate(ctx, id); err != nil {
		return fmt.Errorf("delete certificate: %w", err)
	}
	return nil
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
