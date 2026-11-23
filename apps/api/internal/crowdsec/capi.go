package crowdsec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

const capiOperationKey = "crowdsec_capi_operation"

var ErrCAPIBusy = errors.New("a Central API change is already in progress")

type CAPISharing struct {
	Manual  bool `json:"share_manual_decisions"`
	Custom  bool `json:"share_custom"`
	Tainted bool `json:"share_tainted"`
	Context bool `json:"share_context"`
}

type CAPIChange struct {
	RestartConsole bool         `json:"restart_console,omitempty"`
	Enabled        *bool        `json:"enabled,omitempty"`
	Sharing        *CAPISharing `json:"sharing,omitempty"`
}

type CAPIOperation struct {
	Phase     string    `json:"phase"`
	Reason    string    `json:"reason,omitempty"`
	Reverted  bool      `json:"reverted"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CAPIStatus struct {
	Enrollment       CAPIEnrollment       `json:"enrollment"`
	State            string               `json:"state"`
	Busy             bool                 `json:"busy"`
	Phase            string               `json:"phase"`
	Reason           string               `json:"reason,omitempty"`
	Health           string               `json:"health"`
	Sharing          CAPISharing          `json:"sharing"`
	SharingAvailable bool                 `json:"sharing_available"`
	Connection       *CAPIConnectionCheck `json:"connection,omitempty"`
}

type CAPIStore interface {
	GetSetting(context.Context, string) (generated.Setting, error)
	UpsertSetting(context.Context, generated.UpsertSettingParams) (generated.Setting, error)
}

// CAPIManager serializes local CAPI changes and deliberate upstream actions.
type CAPIManager struct {
	rt            runtime.ContainerRuntime
	runner        Runner
	store         CAPIStore
	queueMu       sync.Mutex
	applyMu       sync.Mutex
	checkMu       sync.Mutex
	healthTimeout time.Duration
	pollInterval  time.Duration
}

func NewCAPIManager(rt runtime.ContainerRuntime, store CAPIStore) *CAPIManager {
	return &CAPIManager{rt: rt, runner: NewContainerRunner(rt), store: store, healthTimeout: 90 * time.Second, pollInterval: 2 * time.Second}
}

func (m *CAPIManager) operation(ctx context.Context) (CAPIOperation, error) {
	row, err := m.store.GetSetting(ctx, capiOperationKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return CAPIOperation{}, nil
	}
	if err != nil {
		return CAPIOperation{}, err
	}
	var state CAPIOperation
	if err := json.Unmarshal([]byte(row.Value), &state); err != nil {
		return CAPIOperation{}, errors.New("stored CAPI operation is invalid")
	}
	return state, nil
}

func (m *CAPIManager) saveOperation(ctx context.Context, state CAPIOperation) error {
	state.UpdatedAt = time.Now().UTC()
	state.Reason = redact.Error(state.Reason)
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = m.store.UpsertSetting(ctx, generated.UpsertSettingParams{Key: capiOperationKey, Value: string(raw)})
	return err
}

// Queue persists pending state before enqueueing, under the same short lock the
// worker takes at startup. A failed enqueue restores the previous UI state.
func (m *CAPIManager) Queue(ctx context.Context, change CAPIChange, enqueue func([]byte) error) error {
	if change.Enabled == nil && change.Sharing == nil && !change.RestartConsole {
		return errors.New("an enabled state or sharing preferences are required")
	}
	if change.RestartConsole && (change.Enabled != nil || change.Sharing != nil) {
		return errors.New("restart console enrollment separately from changing Central API settings")
	}
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	previous, err := m.operation(ctx)
	if err != nil {
		return err
	}
	if previous.Phase == "queued" || previous.Phase == "applying" {
		return ErrCAPIBusy
	}
	if change.RestartConsole {
		if err := m.capiCooldown(ctx); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(change)
	if err != nil {
		return err
	}
	if err := m.saveOperation(ctx, CAPIOperation{Phase: "queued"}); err != nil {
		return err
	}
	if err := enqueue(raw); err != nil {
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		restoreErr := m.saveOperation(restoreCtx, previous)
		return errors.Join(err, restoreErr)
	}
	return nil
}

func (m *CAPIManager) container(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	containers, err := m.rt.ListSystemContainers(ctx)
	if err != nil {
		return "", err
	}
	name := ""
	for _, container := range containers {
		if container.Labels["belune-system"] != SystemLabel {
			continue
		}
		if name != "" {
			return "", errors.New("multiple CrowdSec containers were found; refusing an ambiguous configuration change")
		}
		name = container.Name
	}
	if name == "" {
		return "", ErrUnavailable
	}
	return name, nil
}

// console status authenticates when CAPI is configured, even if it exits 0
// after an upstream error. There must be no command fallback for a failed read.
func (m *CAPIManager) sharing(ctx context.Context, name string, config map[string]any) (CAPISharing, error) {
	path := "/etc/crowdsec/console.yaml"
	api, _ := config["api"].(map[string]any)
	server, _ := api["server"].(map[string]any)
	if configured, exists := server["console_path"]; exists && configured != nil {
		value, ok := configured.(string)
		if !ok {
			return CAPISharing{}, errors.New("CrowdSec console path is not a string")
		}
		if value != "" {
			path = value
		}
	}
	raw, err := m.readLocalFile(ctx, name, path)
	if err != nil {
		return CAPISharing{}, err
	}
	return parseCAPISharing(raw)
}

func parseCAPISharing(raw []byte) (CAPISharing, error) {
	var options struct {
		Manual  *bool `yaml:"share_manual_decisions"`
		Custom  *bool `yaml:"share_custom"`
		Tainted *bool `yaml:"share_tainted"`
		Context *bool `yaml:"share_context"`
	}
	if err := yaml.Unmarshal(raw, &options); err != nil {
		return CAPISharing{}, errors.New("could not read CrowdSec sharing preferences")
	}
	// CrowdSec v1.8.1 LoadConsoleConfig defaults each absent/null key separately.
	sharing := CAPISharing{Custom: true, Tainted: true}
	if options.Manual != nil {
		sharing.Manual = *options.Manual
	}
	if options.Custom != nil {
		sharing.Custom = *options.Custom
	}
	if options.Tainted != nil {
		sharing.Tainted = *options.Tainted
	}
	if options.Context != nil {
		sharing.Context = *options.Context
	}
	return sharing, nil
}

func (m *CAPIManager) Status(ctx context.Context) (CAPIStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	operation, err := m.operation(ctx)
	if err != nil {
		return CAPIStatus{}, err
	}
	result := CAPIStatus{State: "not_registered", Phase: operation.Phase, Reason: operation.Reason, Busy: operation.Phase == "queued" || operation.Phase == "applying"}
	result.Enrollment, err = m.enrollmentRecord(ctx)
	if err != nil {
		return CAPIStatus{}, err
	}
	name, err := m.container(ctx)
	if err != nil {
		result.State = "unavailable"
		result.Reason = "The CrowdSec container is unavailable"
		return result, nil
	}
	result.Health, _ = m.rt.ContainerHealth(ctx, name)
	config, localErr := m.localConfig(ctx, name)
	configured := false
	if localErr == nil {
		sharing, sharingErr := m.sharing(ctx, name, config)
		if sharingErr == nil {
			result.Sharing = sharing
			result.SharingAvailable = true
		}
		configured, localErr = m.localStateFromConfig(ctx, name, config)
	}
	if configured && localErr == nil {
		record, bindErr := m.bindEnrollment(ctx, name, result.Enrollment)
		if bindErr != nil {
			result.Enrollment = CAPIEnrollment{State: "not_enrolled", Reason: "The current registration could not be read; enrollment is not confirmed"}
		} else {
			result.Enrollment = record
		}
	}
	result.Connection, err = m.connectionRecord(ctx)
	if err != nil {
		return CAPIStatus{}, err
	}
	switch {
	case configured && localErr == nil && result.Health == "healthy":
		result.State = "enabled"
	case operation.Reason != "" && operation.Reverted && result.Health == "healthy":
		result.State = "failed_reverted"
	case operation.Reason != "" && result.Health != "healthy":
		result.State = "failed"
	case localErr != nil && configured:
		result.State = "failed"
		if result.Reason == "" {
			result.Reason = "Central API local credentials are missing, invalid or unsupported"
		}
	case localErr != nil:
		result.State = "unavailable"
		if result.Reason == "" {
			result.Reason = "Central API local configuration could not be read or is incomplete"
		}
	case result.Health != "healthy":
		result.State = "unavailable"
	default:
		result.State = "not_registered"
	}
	return result, nil
}

// Apply outlives HTTP cancellation and gives rollback a separate budget. The
// journal remains on the volume until success or a verified healthy recovery.
func (m *CAPIManager) Apply(ctx context.Context, change CAPIChange) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Minute)
	defer cancel()
	m.queueMu.Lock()
	err := m.saveOperation(ctx, CAPIOperation{Phase: "applying"})
	m.queueMu.Unlock()
	if err != nil {
		return err
	}
	reverted, applyErr := m.apply(ctx, change)
	operation := CAPIOperation{Phase: "idle", Reverted: reverted}
	if applyErr != nil {
		operation.Reason = applyErr.Error()
	}
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	// Operator failures are persisted for polling, rather than retried into a
	// second anonymous registration. Storage failure is retryable by the worker.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer saveCancel()
	return m.saveOperation(saveCtx, operation)
}

func (m *CAPIManager) exec(ctx context.Context, name string, command []string, input string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var stdout, stderr limitedBuffer
	code, err := m.rt.ContainerExec(ctx, name, command, strings.NewReader(input), &stdout, &stderr)
	if err != nil {
		return "", errors.New("CrowdSec command could not run")
	}
	if code != 0 {
		if capiRateLimited(stderr.String()) {
			return "", ErrCAPIRateLimited
		}
		return "", fmt.Errorf("CrowdSec command failed (exit %d)", code)
	}
	return stdout.String(), nil
}

func (m *CAPIManager) waitHealthy(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, m.healthTimeout)
	defer cancel()
	stable := 0
	for {
		health, err := m.rt.ContainerHealth(ctx, name)
		if err == nil && health == "healthy" {
			stable++
		} else {
			stable = 0
		}
		if stable >= 2 {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("CrowdSec did not reach a stable healthy state before the health deadline")
		case <-time.After(m.pollInterval):
		}
	}
}

func (m *CAPIManager) restartAndVerify(ctx context.Context, name string) error {
	restartCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := m.rt.RestartContainer(restartCtx, name, 10)
	cancel()
	if err != nil {
		return fmt.Errorf("restart CrowdSec: %w", err)
	}
	if err := m.waitHealthy(ctx, name); err != nil {
		return err
	}
	if _, err := m.runner.Run(ctx, "lapi", "status"); err != nil {
		return errors.New("CrowdSec local API did not authenticate after restart")
	}
	return nil
}

func (m *CAPIManager) rollback(ctx context.Context, name string, files *capiFiles) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 150*time.Second)
	defer cancel()
	if err := files.restore(ctx); err != nil {
		return err
	}
	if err := m.restartAndVerify(ctx, name); err != nil {
		return err
	}
	return files.commit(ctx)
}

func (m *CAPIManager) apply(ctx context.Context, change CAPIChange) (reverted bool, resultErr error) {
	name, err := m.container(ctx)
	if err != nil {
		return false, err
	}
	if change.RestartConsole {
		// Restarting authenticates CAPI during daemon startup. Do not turn a known
		// IP ban into a fatal local-API restart.
		if err := m.capiCooldown(ctx); err != nil {
			return false, err
		}
		if _, _, err := m.consolePrerequisite(ctx); err != nil {
			return false, err
		}
		record, err := m.enrollmentRecord(ctx)
		if err != nil {
			return false, err
		}
		record, err = m.bindEnrollment(ctx, name, record)
		if err != nil {
			return false, err
		}
		if !record.RestartRequired {
			return false, errors.New("no console enrollment restart is pending")
		}
		if err := m.restartAndVerify(ctx, name); err != nil {
			return false, err
		}
		record.RestartRequired = false
		record.Outcome = ""
		record.Reason = "CrowdSec restarted and the local API is healthy. Check enrollment to confirm the console state"
		return false, m.saveEnrollment(ctx, record)
	}
	files, err := newCAPIFiles(ctx, m.rt, name)
	if err != nil {
		return false, err
	}
	defer files.close(ctx)
	interrupted, err := files.hasJournal(ctx)
	if err != nil {
		return false, err
	}
	if interrupted {
		if err := m.rollback(ctx, name, files); err != nil {
			return false, fmt.Errorf("interrupted CAPI change could not be recovered: %w", err)
		}
		return true, errors.New("an interrupted Central API change was reverted; the local API is healthy")
	}
	health, err := m.rt.ContainerHealth(ctx, name)
	if err != nil || (health != "healthy" && (change.Enabled == nil || *change.Enabled)) || health == "none" {
		return false, errors.New("CrowdSec must be healthy before changing Central API settings")
	}
	if err := files.snapshot(ctx); err != nil {
		return false, err
	}
	local, err := files.read(ctx, "config.yaml.local")
	if err != nil {
		return false, err
	}
	base, err := files.read(ctx, "config.yaml")
	if err != nil {
		return false, err
	}
	temporary, err := registrationConfig(base, local)
	if err != nil {
		return false, err
	}
	var credentials []byte
	if change.Enabled != nil && *change.Enabled {
		// Registration requires online_client even while the live daemon has it
		// disabled. Only this temporary /tmp config gets the preflight stanza.
		raw, err := m.exec(ctx, name, []string{"sh", "-ec", `umask 077; config=$(mktemp /tmp/belune-capi-register.XXXXXX); trap 'rm -f "$config"' EXIT HUP INT TERM; cat > "$config"; cscli -c "$config" capi register --file /dev/stdout`}, string(temporary))
		if err != nil {
			return false, fmt.Errorf("central API registration failed; the live configuration was not changed: %w", err)
		}
		credentials = []byte(raw)
		if err := validateCAPICredentials(credentials); err != nil {
			return false, err
		}
	}
	if change.Enabled == nil && change.Sharing != nil {
		existing, err := files.read(ctx, "online_api_credentials.yaml")
		if err != nil {
			return false, err
		}
		if validateCAPICredentials(existing) != nil {
			return false, errors.New("enable Central API before saving sharing preferences; registration is required")
		}
	}
	if err := files.begin(ctx); err != nil {
		return false, err
	}
	defer func() {
		if resultErr == nil {
			return
		}
		if err := m.rollback(ctx, name, files); err != nil {
			reverted = false
			resultErr = fmt.Errorf("central API change failed and automatic recovery could not be verified; check the local API: %w", err)
		} else {
			reverted = true
			resultErr = fmt.Errorf("central API change failed and was reverted; the local API is healthy: %w", resultErr)
		}
	}()
	if len(credentials) > 0 {
		if err := files.write(ctx, "online_api_credentials.yaml", credentials); err != nil {
			return false, err
		}
	}
	if change.Sharing != nil {
		for _, option := range []struct {
			name    string
			enabled bool
		}{
			{"manual", change.Sharing.Manual}, {"custom", change.Sharing.Custom}, {"tainted", change.Sharing.Tainted}, {"context", change.Sharing.Context},
		} {
			action := "disable"
			if option.enabled {
				action = "enable"
			}
			if _, err := m.exec(ctx, name, []string{"sh", "-ec", `umask 077; config=$(mktemp /tmp/belune-capi-console.XXXXXX); trap 'rm -f "$config"' EXIT HUP INT TERM; cat > "$config"; cscli -c "$config" console "$1" "$2"`, "sh", action, option.name}, string(temporary)); err != nil {
				return false, fmt.Errorf("could not apply %s sharing preference", option.name)
			}
		}
	}
	if change.Enabled != nil {
		override, err := localCAPIConfig(local, *change.Enabled)
		if err != nil {
			return false, err
		}
		if err := files.write(ctx, "config.yaml.local", override); err != nil {
			return false, err
		}
	}
	if err := m.restartAndVerify(ctx, name); err != nil {
		return false, err
	}
	if change.Enabled != nil {
		if *change.Enabled {
			checked, err := m.checkConnection(ctx)
			if err != nil {
				return false, err
			}
			if checked.State != "connected" {
				if checked.State == "rate_limited" {
					return false, ErrCAPIRateLimited
				}
				return false, errors.New(checked.Reason)
			}
		} else {
			configured, err := m.localState(ctx, name)
			if err != nil || configured {
				return false, errors.New("central API disable could not be verified locally")
			}
		}
	}
	if err := files.commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}
