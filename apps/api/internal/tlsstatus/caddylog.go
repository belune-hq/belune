// Package tlsstatus lifts certificate failure reasons out of Caddy's own logs.
//
// Caddy knows exactly why a certificate could not be issued — the ACME server
// told it — but that reason only ever appears in its stdout, where no user of a
// PaaS UI will ever look. The domain just sits on "pending" forever. This is the
// gap the TLS status pipeline exists to close: the log collector already streams
// Caddy's container logs, so the reason is right there to be picked up and
// attached to the domain it belongs to.
package tlsstatus

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

// caddyTLSLogger is the logger name Caddy uses for certificate issuance. Its
// error lines are the ones worth showing a user.
const caddyTLSLogger = "tls.obtain"

// caddyLogLine is the subset of Caddy's JSON log format we care about.
//
// Two shapes occur in practice, both captured from a real failing issuance:
//
//	{"level":"error","logger":"tls.obtain","msg":"could not get certificate from issuer",
//	 "identifier":"app.example.com","error":"HTTP 400 urn:ietf:params:acme:error:..."}
//
//	{"level":"error","logger":"tls.obtain","msg":"will retry",
//	 "error":"[app.example.com] Obtain: registering account ...","attempt":1}
//
// The first names the host in `identifier`; the second only carries it bracketed
// at the front of `error`.
type caddyLogLine struct {
	Level      string `json:"level"`
	Logger     string `json:"logger"`
	Msg        string `json:"msg"`
	Identifier string `json:"identifier"`
	Error      string `json:"error"`
}

// ParseCaddyTLSError extracts the hostname and a human-readable reason from one
// Caddy log line, reporting false for anything that is not a certificate
// failure — which is the overwhelming majority of lines.
func ParseCaddyTLSError(line string) (hostname, reason string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return "", "", false
	}

	var entry caddyLogLine
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return "", "", false
	}
	if entry.Level != "error" || entry.Logger != caddyTLSLogger {
		return "", "", false
	}

	hostname = entry.Identifier
	reason = entry.Error
	if reason == "" {
		reason = entry.Msg
	}

	// "will retry" lines carry no identifier; the host is bracketed in the error.
	if hostname == "" {
		hostname, reason = splitBracketedHost(reason)
	}
	if hostname == "" || reason == "" {
		return "", "", false
	}
	return hostname, cleanACMEReason(reason), true
}

// acmeProblemURN matches the ACME problem type that precedes the CA's own
// explanation, e.g. "…HTTP 400 urn:ietf:params:acme:error:dns - DNS problem: …".
var acmeProblemURN = regexp.MustCompile(`urn:ietf:params:acme:error:[a-zA-Z]+ - `)

// cleanACMEReason reduces Caddy's raw error to the part an operator can act on.
//
// The raw string is layered with protocol wrapping — "Obtain: [host] solving
// challenge: host: [host] authorization failed: HTTP 400 urn:…:error:dns - " —
// before it ever reaches the sentence the CA actually wrote, and the hostname is
// repeated three times along the way. This subsystem exists to hand the operator
// a reason, so hand them the reason.
func cleanACMEReason(reason string) string {
	// certmagic appends the CA it used. After a first failure it deliberately
	// retries against Let's Encrypt's *staging* endpoint, so it does not burn the
	// production failed-validation rate limit on a configuration that is plainly
	// broken. That is correct behaviour, but shown to an operator it reads as
	// "your certificate will be untrusted" — which is not what it means, and not
	// something they can do anything about.
	if i := strings.Index(reason, " (ca=http"); i >= 0 {
		reason = reason[:i]
	}
	// Everything up to and including the problem URN is wrapping; what follows is
	// the CA's own sentence, which is the part worth reading.
	if loc := acmeProblemURN.FindStringIndex(reason); loc != nil {
		reason = reason[loc[1]:]
	}
	return strings.TrimSpace(reason)
}

// splitBracketedHost pulls "app.example.com" out of a leading "[app.example.com] rest…"
// and returns the remainder as the reason.
func splitBracketedHost(s string) (host, rest string) {
	if !strings.HasPrefix(s, "[") {
		return "", s
	}
	end := strings.Index(s, "]")
	if end < 0 {
		return "", s
	}
	host = s[1:end]
	rest = strings.TrimSpace(s[end+1:])
	if rest == "" {
		rest = s
	}
	// A bracketed segment that is not a hostname (Caddy also brackets other
	// things) would attach the error to a domain that does not exist — harmless,
	// since the update matches on hostname, but not worth doing.
	if !strings.Contains(host, ".") {
		return "", s
	}
	return host, rest
}

// Recorder attaches a parsed failure reason to the domain it names.
type Recorder struct {
	queries *generated.Queries
}

func NewRecorder(queries *generated.Queries) *Recorder {
	return &Recorder{queries: queries}
}

// HandleCaddyLine is the log-collector hook. It is called for every line Caddy
// emits, so it must be cheap for the common case: ParseCaddyTLSError rejects
// non-JSON and non-issuance lines before doing any work, and only a genuine
// failure reaches the database.
func (r *Recorder) HandleCaddyLine(ctx context.Context, line string) {
	hostname, reason, ok := ParseCaddyTLSError(line)
	if !ok {
		return
	}

	r.Record(ctx, hostname, reason)
}

// Record attaches a failure reason to a domain. Besides the Caddy log hook, this
// is the sink for a failed SetupTLS — which used to be a slog.Warn and nothing
// else, so the route went live and the UI showed no hint that HTTPS was broken.
func (r *Recorder) Record(ctx context.Context, hostname, reason string) {
	if _, err := r.queries.GetDomainByHostname(ctx, hostname); err != nil {
		// Caddy may be serving hostnames we no longer know about; that is not an
		// error worth surfacing.
		slog.Debug("tls status: failure reported for an unknown domain", "hostname", hostname)
		return
	}

	// Recorded against the hostname, so every path sharing it reports the failure.
	// Caddy fails to get a certificate for a *name*; a sibling row still claiming
	// HTTPS is fine would be a lie about the same certificate.
	if err := r.queries.SetDomainTLSError(ctx, generated.SetDomainTLSErrorParams{
		Hostname: hostname,
		TlsError: pgText(reason),
	}); err != nil {
		slog.Warn("tls status: failed to record TLS error", "hostname", hostname, "error", err)
		return
	}
	slog.Info("tls status: recorded certificate failure", "hostname", hostname, "reason", reason)
}

func pgText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}
