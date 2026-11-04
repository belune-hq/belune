package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
)

// Process type errors the handler maps to distinct statuses. The two uniqueness
// errors are DIFFERENT conditions with different messages, and are told apart by the
// name of the index that fired, never by guessing: telling a user who added a second
// routable type that their NAME is taken would be actively misleading.
var (
	ErrProcessTypeNotFound   = errors.New("process type not found")
	ErrProcessTypeNameTaken  = errors.New("a process type with this name already exists on this application")
	ErrRoutableTypeExists    = errors.New("this application already has a routable process type")
	ErrRoutableTypeProtected = errors.New("the routable process type cannot be deleted: every domain of this application routes to it")
)

// The unique indexes migration 000079 created. pgconn reports the index's name for a
// unique violation, which is what distinguishes the two cases.
const (
	processTypeNameIndex     = "idx_application_process_types_name"
	processTypeRoutableIndex = "idx_application_process_types_routable"
)

// ProcessTypeValidationError is a request the caller can fix; the handler answers 400.
type ProcessTypeValidationError struct{ Msg string }

func (e *ProcessTypeValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error {
	return &ProcessTypeValidationError{Msg: fmt.Sprintf(format, a...)}
}

// processTypeNameRe is the same pattern migration 000079 enforces as a CHECK. It is
// repeated here only so the caller gets a 400 with a reason rather than a constraint
// violation; the database remains the authority.
var processTypeNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

const (
	maxProcessCommandLen = 4096
	maxHealthCommandLen  = 1000
	// dockerNameLimit is Docker's container-name limit. project.slug and the base slug
	// are each VARCHAR(255), so an application slug can already approach it before any
	// suffix; a name that cannot be created must fail here, as a 400, not later as a
	// Docker error at container-create time.
	dockerNameLimit = 255
)

// ProcessTypeInput is what a caller may set when creating a type. Replicas and the
// per-type CPU/memory limits are deliberately absent: nothing reads them in 0.3.0
// (replicas are fixed at 1 and a deploy applies only the application's limits), and a
// setting that is stored but silently ignored is worse than no setting.
type ProcessTypeInput struct {
	Name               string
	Command            string
	Routable           bool
	HealthCheckType    string // "" = resolve by the rule: routable inherits, else none
	HealthCheckCommand string // "" = inherit the application's
}

// ProcessTypePatch is a partial update. A nil field is ABSENT and keeps the stored
// value; an explicit empty string clears HealthCheckType / HealthCheckCommand back
// to NULL. (A JSON null is also nil, matching how every other optional field in this
// API behaves: only an explicit empty value clears.) Name and routable are not
// patchable: the name is an identity, and moving the routable flag moves the proxy's
// upstream.
type ProcessTypePatch struct {
	Command            *string
	HealthCheckType    *string
	HealthCheckCommand *string
}

// validateHealth enforces the one rule the deploy cannot honour: a worker set to
// 'http' verifies nothing, because only a command check gates a non-routable type.
func validateHealth(app generated.Application, routable bool, healthType, healthCommand string) error {
	switch healthType {
	case "":
	case "none", "command":
	case "http":
		if !routable {
			return invalid("health_check_type 'http' needs a port and domains, so it is only valid on the routable process type; use 'command' or 'none'")
		}
	default:
		return invalid("health_check_type must be one of http, command, none")
	}
	if len(healthCommand) > maxHealthCommandLen {
		return invalid("health_check_command is too long")
	}
	// A command check with no command is a no-op that would look configured.
	if healthType == "command" {
		effective := healthCommand
		if effective == "" && app.HealthCheckCommand.Valid {
			effective = app.HealthCheckCommand.String
		}
		if strings.TrimSpace(effective) == "" {
			return invalid("health_check_type 'command' needs a health_check_command (the application has none to inherit)")
		}
	}
	return nil
}

func validateCommand(command string) error {
	if len(command) > maxProcessCommandLen {
		return invalid("command is too long")
	}
	if strings.ContainsRune(command, 0) {
		return invalid("command must not contain a NUL byte")
	}
	return nil
}

// ListProcessTypes returns an application's process types with the instance each owns.
func (s *ApplicationService) ListProcessTypes(ctx context.Context, appID pgtype.UUID) ([]generated.ApplicationProcessType, map[pgtype.UUID][]generated.ApplicationInstance, error) {
	types, err := s.queries.ListProcessTypesByApplication(ctx, appID)
	if err != nil {
		return nil, nil, fmt.Errorf("listing process types: %w", err)
	}
	insts, err := s.queries.ListInstancesByApplication(ctx, appID)
	if err != nil {
		return nil, nil, fmt.Errorf("listing instances: %w", err)
	}
	byType := make(map[pgtype.UUID][]generated.ApplicationInstance, len(types))
	for _, in := range insts {
		byType[in.ProcessTypeID] = append(byType[in.ProcessTypeID], in)
	}
	return types, byType, nil
}

// CreateProcessType creates the type and its one instance row together.
//
// This is the first place a container name is MINTED rather than read:
// {appSlug}-{processType}-{idx}. The deployment-id suffix A1 recorded for the full
// format ({appSlug}-{processType}-{idx}-{deploymentID[:8]}) is left off on purpose.
// It exists only so an old and a new web-1 can coexist during a rolling deploy;
// 0.3.0 has none, and a type created here has no deployment to name it after. Names
// are DATA on the row, so 0.3.1 can mint suffixed names for new generations with no
// migration and no rename. Nothing is deployed by this call: the container appears
// on the next deploy.
func (s *ApplicationService) CreateProcessType(ctx context.Context, appID pgtype.UUID, in ProcessTypeInput) (generated.ApplicationProcessType, generated.ApplicationInstance, error) {
	if !processTypeNameRe.MatchString(in.Name) {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, invalid("name must start with a lowercase letter and contain only lowercase letters, digits and '-' (at most 20 characters)")
	}
	if err := validateCommand(in.Command); err != nil {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, err
	}
	app, err := s.queries.GetApplication(ctx, appID)
	if err != nil {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, fmt.Errorf("loading application: %w", err)
	}
	if err := validateHealth(app, in.Routable, in.HealthCheckType, in.HealthCheckCommand); err != nil {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, err
	}

	const idx = 1 // replicas are fixed at 1 in 0.3.0
	containerName := fmt.Sprintf("%s-%s-%d", app.Slug, in.Name, idx)
	if len(containerName) > dockerNameLimit {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, invalid(
			"the container name %q would be %d characters, over Docker's %d limit; use a shorter process type name or application name", containerName, len(containerName), dockerNameLimit)
	}

	var pt generated.ApplicationProcessType
	var inst generated.ApplicationInstance
	err = store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		var err error
		pt, err = q.CreateProcessType(ctx, generated.CreateProcessTypeParams{
			ApplicationID:      appID,
			Name:               in.Name,
			Command:            in.Command,
			Replicas:           1,
			Routable:           in.Routable,
			HealthCheckType:    optText(in.HealthCheckType),
			HealthCheckCommand: optText(in.HealthCheckCommand),
		})
		if err != nil {
			return err
		}
		inst, err = q.CreateInstance(ctx, generated.CreateInstanceParams{
			ApplicationID: appID, ProcessTypeID: pt.ID, Idx: idx, ContainerName: containerName, Status: "pending",
		})
		return err
	})
	if err != nil {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, mapProcessTypeUniqueViolation(err)
	}
	s.MarkConfigChanged(ctx, appID)
	return pt, inst, nil
}

// UpdateProcessType applies a patch.
//
// ⛔ UpdateProcessType (the SQL) is a FULL-ROW update, so this reads the stored row
// first and falls back to it for EVERY column the statement writes: command, replicas,
// routable, cpu_limit, memory_limit, health_check_type and health_check_command. A
// partial body passed straight through would silently clear the command, flip
// routable to false and null both health columns. The columns the API does not expose
// (replicas and the limits) are carried over from the row, not written from a default.
func (s *ApplicationService) UpdateProcessType(ctx context.Context, appID, typeID pgtype.UUID, p ProcessTypePatch) (generated.ApplicationProcessType, generated.ApplicationInstance, error) {
	cur, err := s.queries.GetProcessType(ctx, typeID)
	if err != nil || cur.ApplicationID != appID {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, ErrProcessTypeNotFound
	}
	app, err := s.queries.GetApplication(ctx, appID)
	if err != nil {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, fmt.Errorf("loading application: %w", err)
	}

	command := cur.Command
	if p.Command != nil {
		command = *p.Command
	}
	healthType, healthCommand := cur.HealthCheckType, cur.HealthCheckCommand
	if p.HealthCheckType != nil {
		healthType = optText(*p.HealthCheckType)
	}
	if p.HealthCheckCommand != nil {
		healthCommand = optText(*p.HealthCheckCommand)
	}

	if err := validateCommand(command); err != nil {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, err
	}
	if err := validateHealth(app, cur.Routable, healthType.String, healthCommand.String); err != nil {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, err
	}

	pt, err := s.queries.UpdateProcessType(ctx, generated.UpdateProcessTypeParams{
		ID:                 typeID,
		Command:            command,
		Replicas:           cur.Replicas,
		Routable:           cur.Routable,
		CpuLimit:           cur.CpuLimit,
		MemoryLimit:        cur.MemoryLimit,
		HealthCheckType:    healthType,
		HealthCheckCommand: healthCommand,
	})
	if err != nil {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, mapProcessTypeUniqueViolation(err)
	}
	insts, err := s.queries.ListInstancesByProcessType(ctx, typeID)
	if err != nil {
		return generated.ApplicationProcessType{}, generated.ApplicationInstance{}, fmt.Errorf("listing instances: %w", err)
	}
	var inst generated.ApplicationInstance
	if len(insts) > 0 {
		inst = insts[0]
	}
	s.MarkConfigChanged(ctx, appID)
	return pt, inst, nil
}

// DeleteProcessType removes a non-routable type, its instance row (cascade) and its
// container. The routable type is refused outright, not merely while domains exist:
// deleting it removes the instance the proxy routes to, the reconciler would then HOLD
// the application's routes while the container is gone, and every domain would 502
// for good. The backfill guarantees exactly one routable type and nothing in the model
// copes with zero.
//
// The container is removed AFTER the row is deleted, best-effort, and a failure is
// returned as a warning rather than an error: the row is already gone, so retrying the
// delete is impossible, and the orphan reaper (which claims names from instance rows,
// so no longer claims this one) remains the backstop. Docker calls are deliberately not
// made inside the database transaction.
func (s *ApplicationService) DeleteProcessType(ctx context.Context, appID, typeID pgtype.UUID) (warning string, err error) {
	cur, err := s.queries.GetProcessType(ctx, typeID)
	if err != nil || cur.ApplicationID != appID {
		return "", ErrProcessTypeNotFound
	}
	if cur.Routable {
		return "", ErrRoutableTypeProtected
	}
	insts, err := s.queries.ListInstancesByProcessType(ctx, typeID)
	if err != nil {
		return "", fmt.Errorf("listing instances: %w", err)
	}

	if err := store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		return q.DeleteProcessType(ctx, typeID)
	}); err != nil {
		return "", fmt.Errorf("deleting process type: %w", err)
	}
	s.MarkConfigChanged(ctx, appID)

	rt, rtErr := RuntimeForApplication(ctx, s.queries, s.runtimes, appID)
	if rtErr != nil {
		slog.Warn("delete process type: could not reach the application's server", "process_type", cur.Name, "error", rtErr)
		return "process type deleted, but its container could not be removed yet (its server was unreachable); it will be reclaimed by the orphan sweep", nil
	}
	for _, in := range insts {
		if err := rt.StopContainer(ctx, in.ContainerName); err != nil {
			slog.Debug("delete process type: could not stop container (may not exist)", "container", in.ContainerName, "error", err)
		}
		if err := rt.RemoveContainer(ctx, in.ContainerName); err != nil {
			slog.Warn("delete process type: could not remove container", "container", in.ContainerName, "error", err)
			warning = "process type deleted, but its container could not be removed yet; it will be reclaimed by the orphan sweep"
		}
	}
	return warning, nil
}

// mapProcessTypeUniqueViolation turns a unique-index violation into the error for
// the index that fired. ConstraintName is what tells "this name is taken" from "this
// application already has a routable type"; the index is the only authority, so this
// mapping is the backstop for any pre-check, which would race.
func mapProcessTypeUniqueViolation(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case processTypeNameIndex:
			return ErrProcessTypeNameTaken
		case processTypeRoutableIndex:
			return ErrRoutableTypeExists
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrProcessTypeNotFound
	}
	return err
}

func optText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
