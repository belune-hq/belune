package crowdsec

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

const capiEnrollmentKey = "crowdsec_capi_enrollment"

var errConsoleTimeout = errors.New("console enrollment command timed out")
var ErrCAPIEnrollmentInvalid = errors.New("invalid console enrollment request")

// Only the enrollment marker is persisted. Neither a one-shot key nor a quick
// approval URL belongs in settings, jobs, audit details or error messages.
type CAPIEnrollment struct {
	watcher         string     `json:"-"`
	State           string     `json:"state"`
	Outcome         string     `json:"outcome,omitempty"`
	Reason          string     `json:"reason,omitempty"`
	RequestedAt     *time.Time `json:"requested_at,omitempty"`
	CheckedAt       *time.Time `json:"checked_at,omitempty"`
	RestartRequired bool       `json:"restart_required"`
	URL             string     `json:"url,omitempty"`
}
type CAPIEnrollRequest struct {
	Mode      string       `json:"mode"`
	Key       string       `json:"key,omitempty"`
	Overwrite bool         `json:"overwrite"`
	Consent   bool         `json:"consent"`
	Sharing   *CAPISharing `json:"sharing"`
}

func (m *CAPIManager) enrollmentRecord(ctx context.Context) (CAPIEnrollment, error) {
	row, err := m.store.GetSetting(ctx, capiEnrollmentKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return CAPIEnrollment{State: "not_enrolled"}, nil
	}
	if err != nil {
		return CAPIEnrollment{}, err
	}
	var stored struct {
		CAPIEnrollment
		Watcher string `json:"watcher"`
	}
	if json.Unmarshal([]byte(row.Value), &stored) != nil {
		return CAPIEnrollment{}, errors.New("stored console enrollment is invalid")
	}
	stored.watcher = stored.Watcher
	return stored.CAPIEnrollment, nil
}
func (m *CAPIManager) saveEnrollment(ctx context.Context, record CAPIEnrollment) error {
	record.URL = ""
	raw, err := json.Marshal(struct {
		CAPIEnrollment
		Watcher string `json:"watcher"`
	}{record, record.watcher})
	if err != nil {
		return err
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, err = m.store.UpsertSetting(saveCtx, generated.UpsertSettingParams{Key: capiEnrollmentKey, Value: string(raw)})
	return err
}

// Re-enabling Tier 1 registers a new watcher. A local marker for the old
// identity cannot establish enrollment of the new one. Hash only the public
// watcher login, not its password; polling still reads local files exclusively.
func (m *CAPIManager) bindEnrollment(ctx context.Context, name string, record CAPIEnrollment) (CAPIEnrollment, error) {
	raw, err := m.readLocalFile(ctx, name, onlineCredentialsPath)
	if err != nil {
		return record, err
	}
	if validateCAPICredentials(raw) != nil {
		return CAPIEnrollment{}, errors.New("central API registration could not be read")
	}
	var credentials capiCredentials
	if yaml.Unmarshal(raw, &credentials) != nil {
		return CAPIEnrollment{}, errors.New("central API registration could not be read")
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(credentials.Login)))
	if record.watcher != identity && (record.State != "not_enrolled" || record.RequestedAt != nil || record.CheckedAt != nil) {
		record = CAPIEnrollment{State: "not_enrolled", Reason: "Central API registration changed. Check or request enrollment for the current registration"}
	}
	record.watcher = identity
	return record, nil
}

// Share the Tier 1 lock and cooldown: switching buttons must not evade an IP ban.
func (m *CAPIManager) beginConsoleAction(ctx context.Context) (func(), error) {
	if !m.checkMu.TryLock() {
		return nil, ErrCAPIBusy
	}
	if !m.queueMu.TryLock() {
		m.checkMu.Unlock()
		return nil, ErrCAPIBusy
	}
	release := func() { m.queueMu.Unlock(); m.checkMu.Unlock() }
	op, err := m.operation(ctx)
	if err == nil && (op.Phase == "queued" || op.Phase == "applying") {
		err = ErrCAPIBusy
	}
	if err == nil {
		err = m.capiCooldown(ctx)
	}

	if err != nil {
		release()
		return nil, err
	}
	return release, nil
}
func (m *CAPIManager) capiCooldown(ctx context.Context) error {
	previous, err := m.connectionRecord(ctx)
	if err != nil {
		return err
	}
	if previous != nil && previous.State == "rate_limited" && time.Since(previous.CheckedAt) < time.Hour {
		return ErrCAPIRateLimited
	}
	return nil
}
func (m *CAPIManager) consolePrerequisite(ctx context.Context) (string, map[string]any, error) {
	name, err := m.container(ctx)
	if err != nil {
		return "", nil, err
	}
	health, err := m.rt.ContainerHealth(ctx, name)
	if err != nil || health != "healthy" {
		return "", nil, errors.New("wait for the CrowdSec local API to become healthy before managing console enrollment")
	}
	config, err := m.localConfig(ctx, name)
	if err != nil {
		return "", nil, errors.New("CrowdSec local configuration could not be read")
	}
	enabled, err := m.localStateFromConfig(ctx, name, config)
	if err != nil {
		return "", nil, errors.New("central API registration is missing or invalid; enable Central API and verify its connection first")
	}
	if !enabled {
		return "", nil, errors.New("enable Central API before enrolling: the live configuration has no Central API connection")
	}
	return name, config, nil
}

// execConsole captures both streams, because enroll logs its result to stderr
// and console status may exit zero after a failed upstream login.
func (m *CAPIManager) execConsole(ctx context.Context, name string, cmd []string, key string) (string, string, error) {
	var stdout, stderr limitedBuffer
	code, err := m.rt.ContainerExec(ctx, name, cmd, nil, &stdout, &stderr)
	out, diagnostic := stdout.String(), stderr.String()
	// Never return arbitrary CLI text. Apply both exact-key and generic redaction
	// before inspecting diagnostics, including runtime errors which may echo argv.
	diagnostic += "\n" + out
	if key != "" {
		diagnostic = strings.ReplaceAll(diagnostic, key, "[REDACTED]")
	}
	diagnostic = redact.Error(diagnostic)
	if consoleRateLimit.MatchString(diagnostic) {
		check := CAPIConnectionCheck{State: "rate_limited", Reason: ErrCAPIRateLimited.Error(), CheckedAt: time.Now().UTC()}
		if saveErr := m.saveConnectionRecord(ctx, check); saveErr != nil {
			return "", "", saveErr
		}
		return "", "", ErrCAPIRateLimited
	}
	if code == 124 || code == 137 || code == 143 {
		return "", "", errConsoleTimeout
	}
	if err != nil || code != 0 {
		return "", "", errors.New("CrowdSec console command failed; verify Central API is enabled, registered and reachable before trying again")
	}
	return out, stderr.String(), nil
}

// A quick-link token may itself contain 403; only error-shaped diagnostics
// identify a rate limit here. console status can emit these with exit zero.
var consoleRateLimit = regexp.MustCompile(`(?i)(http[ /:]+403\b|\b403[ :]+forbidden\b|api error:[ ]*forbidden)`)

var consoleANSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func quickEnrollmentURL(output string) string {
	for _, line := range strings.Split(consoleANSI.ReplaceAllString(output, ""), "\n") {
		if !strings.Contains(line, "enroll your instance") {
			continue
		}
		for _, token := range strings.Fields(line) {
			token = strings.Trim(token, `"'`)
			if !strings.HasPrefix(token, "https://app.crowdsec.net/") {
				continue
			}
			u, err := url.Parse(token)
			if err == nil && u.Scheme == "https" && u.Host == "app.crowdsec.net" && u.User == nil && u.Path != "" && u.Path != "/" {
				return token
			}
			return ""
		}
	}
	return ""
}

func (m *CAPIManager) Enroll(ctx context.Context, request CAPIEnrollRequest) (CAPIEnrollment, error) {
	if !request.Consent || request.Sharing == nil || (request.Mode != "quick" && request.Mode != "key") || (request.Mode == "quick" && (request.Key != "" || request.Overwrite)) || (request.Mode == "key" && (strings.TrimSpace(request.Key) == "" || len(request.Key) > 4096 || strings.ContainsAny(request.Key, "\x00\r\n"))) {
		return CAPIEnrollment{}, ErrCAPIEnrollmentInvalid
	}
	// The one-shot operation survives an HTTP disconnect but cannot outlive the
	// handler's 15s budget; there is deliberately no retry or secret-bearing job.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Second)
	defer cancel()
	release, err := m.beginConsoleAction(ctx)
	if err != nil {
		return CAPIEnrollment{}, err
	}
	defer release()
	name, config, err := m.consolePrerequisite(ctx)
	if err != nil {
		return CAPIEnrollment{}, err
	}
	sharing, err := m.sharing(ctx, name, config)
	if err != nil {
		return CAPIEnrollment{}, err
	}
	// Consent names the currently saved options. Requiring an exact match prevents
	// an old dialog (or CLI defaults) silently broadening sharing during enrollment.
	if sharing != *request.Sharing {
		return CAPIEnrollment{}, errors.New("sharing preferences changed; review the current preferences before enrolling")
	}
	record, err := m.enrollmentRecord(ctx)
	if err != nil {
		return record, err
	}
	record, err = m.bindEnrollment(ctx, name, record)
	if err != nil {
		return record, err
	}
	args := []string{"sh", "-ec", `exec timeout -s TERM 9 cscli --color no -o human console enroll "$@"`, "sh"}
	if row, readErr := m.store.GetSetting(ctx, "instance_name"); readErr == nil && strings.TrimSpace(row.Value) != "" {
		args = append(args, "--name", row.Value)
	} else if readErr != nil && !errors.Is(readErr, pgx.ErrNoRows) {
		return record, readErr
	}
	for _, option := range []struct {
		name    string
		enabled bool
	}{{"manual", sharing.Manual}, {"custom", sharing.Custom}, {"tainted", sharing.Tainted}, {"context", sharing.Context}} {
		flag := "--disable"
		if option.enabled {
			flag = "--enable"
		}
		args = append(args, flag, option.name)
	}
	if request.Mode == "quick" {
		args = append(args, "--quick")
	} else {
		if request.Overwrite {
			args = append(args, "--overwrite")
		}
		args = append(args, "--", request.Key)
	}
	out, diagnostic, err := m.execConsole(ctx, name, args, request.Key)
	if err != nil {
		// A timeout may follow a successful upstream request. Keep an honest local
		// marker so the operator checks before sending another enrollment request.
		if ctx.Err() != nil || errors.Is(err, errConsoleTimeout) {
			record.State = "awaiting_approval"
			now := time.Now().UTC()
			record.RequestedAt = &now
			record.RestartRequired = true
			record.Outcome = "unknown"
			record.Reason = "The enrollment request timed out; check enrollment before trying again"
			return record, m.saveEnrollment(ctx, record)
		}
		return record, err
	}
	output := out + "\n" + diagnostic
	if strings.Contains(strings.ToLower(output), "already enrolled") {
		record.Outcome = "already_enrolled"
		record.Reason = "CrowdSec reports this instance is already enrolled. Check enrollment, or use an enrollment key with explicit overwrite confirmation to replace it"
		return record, m.saveEnrollment(ctx, record)
	}
	now := time.Now().UTC()
	record = CAPIEnrollment{watcher: record.watcher, State: "awaiting_approval", RequestedAt: &now, RestartRequired: true, Reason: "Accept the enrollment in your CrowdSec console, then restart CrowdSec and check enrollment"}
	if request.Mode == "quick" {
		record.URL = quickEnrollmentURL(output)
		if record.URL == "" {
			record.Outcome = "link_unreadable"
			record.Reason = "The approval link could not be read. Use an enrollment key instead; approval may still be pending in your console"
		}
	} else if !strings.Contains(output, "Watcher successfully enrolled") {
		record.Outcome = "unknown"
		record.Reason = "CrowdSec's enrollment response could not be confirmed. Check your console before trying again"
	}
	return record, m.saveEnrollment(ctx, record)
}

// CheckEnrollment is never used by Status, a timer, or a retry. Even a successful
// cscli exit is insufficient: require an authenticated, unambiguous connection table.
func (m *CAPIManager) CheckEnrollment(ctx context.Context) (CAPIEnrollment, error) {
	release, err := m.beginConsoleAction(ctx)
	if err != nil {
		return CAPIEnrollment{}, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	name, _, err := m.consolePrerequisite(ctx)
	if err != nil {
		return CAPIEnrollment{}, err
	}
	record, err := m.enrollmentRecord(ctx)
	if err != nil {
		return record, err
	}
	record, err = m.bindEnrollment(ctx, name, record)
	if err != nil {
		return record, err
	}
	out, _, err := m.execConsole(ctx, name, []string{"timeout", "-s", "TERM", "9", "cscli", "--warning", "--color", "no", "console", "status", "-o", "human"}, "")
	if err != nil {
		return record, err
	}
	enrolled, err := consoleEnrollmentResult(out)
	if err != nil {
		return record, err
	}
	now := time.Now().UTC()
	record.CheckedAt = &now
	record.Outcome = ""
	record.Reason = "Waiting for approval in your CrowdSec console"
	if enrolled {
		record.State = "enrolled"
		record.Reason = "CrowdSec confirmed enrollment"
	} else if record.State != "awaiting_approval" {
		record.State = "not_enrolled"
		record.Reason = "CrowdSec has not confirmed enrollment"
	}
	return record, m.saveEnrollment(ctx, record)
}

// JSON output unconditionally suppresses warnings in cscli v1.8.1 (even with
// --warning), hiding 403s behind authenticated:false and exit 0. Read the
// color-free human connection table instead, with exactly one login. Unknown
// rows, missing authentication, or changed enrollment prose fail closed.
func consoleEnrollmentResult(output string) (bool, error) {
	values := map[string]string{}
	invalid := errors.New("CrowdSec's authenticated enrollment result could not be read; enrollment was not confirmed")
	for _, line := range strings.Split(consoleANSI.ReplaceAllString(output, ""), "\n") {
		cells := strings.Split(strings.TrimSpace(line), "|")
		if len(cells) != 4 {
			continue
		}
		name := strings.TrimSpace(cells[1])
		value := strings.TrimSpace(cells[2])
		if name != "Central API (CAPI)" && name != "Enrolled" {
			continue
		}
		if _, exists := values[name]; exists {
			return false, invalid
		}
		values[name] = value
	}
	if values["Central API (CAPI)"] != "✅ authenticated" {
		return false, invalid
	}
	switch values["Enrolled"] {
	case "✅ enrolled":
		return true, nil
	case "❌ not enrolled":
		return false, nil
	default:
		return false, invalid
	}
}
