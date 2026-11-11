package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

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

// ProcessTypeInput is what a caller may set when creating a type. Replicas nil means
// 1; the limits nil mean "inherit the application's".
type ProcessTypeInput struct {
	Name               string
	Command            string
	Routable           bool
	HealthCheckType    string // "" = resolve by the rule: routable inherits, else none
	HealthCheckCommand string // "" = inherit the application's
	Replicas           *int32
	CPULimit           *float64 // cores; 0 or nil = inherit
	MemoryLimit        *int64   // bytes; 0 or nil = inherit
}

// ProcessTypePatch is a partial update. A nil field is ABSENT and keeps the stored
// value; an explicit empty string clears HealthCheckType / HealthCheckCommand back
// to NULL, and an explicit 0 clears a limit back to NULL (inherit). (A JSON null is
// also nil, matching how every other optional field in this API behaves: only an
// explicit empty value clears.) Name and routable are not patchable: the name is an
// identity, and moving the routable flag moves the proxy's upstream.
type ProcessTypePatch struct {
	Command            *string
	HealthCheckType    *string
	HealthCheckCommand *string
	Replicas           *int32
	CPULimit           *float64
	MemoryLimit        *int64
}

// ScalePlan is what the caller must do to the CONTAINERS after the rows changed: the
// service only moves rows. Surplus containers are named here because their rows are
// already gone and nothing else remembers the names.
type ScalePlan struct {
	ApplicationID    pgtype.UUID
	ProcessTypeID    pgtype.UUID
	RemoveContainers []string
	// CreateContainers is true when rows were minted: their containers are created from
	// the last successful deployment's image, if there is one.
	CreateContainers bool
}

// Empty reports whether there is nothing to do to any container.
func (p ScalePlan) Empty() bool { return len(p.RemoveContainers) == 0 && !p.CreateContainers }

const (
	maxReplicas = 20 // the CHECK in migration 000079
	// minMemoryLimit is Docker's floor; below it container create fails far from here.
	minMemoryLimit = 6 << 20
)

// validateScale holds the rules for how many instances a type may have.
//
// replicas = 0 is "paused" and is legal on a worker -- that is how you pause one
// without deleting its config. It is REFUSED on the routable type, for the reason
// DeleteProcessType refuses to delete it: at zero there are no instances, the upstream
// list is empty, the proxy build refuses an empty route, and the reconciler would hold
// the application's routes while nothing serves them. Pausing it is the same hole by
// another name.
//
// replicas > 1 is refused for EVERY type of an application that has any volume, not
// just the "type with volumes": volumes belong to the application and are mounted into
// every process type's container, so two containers of any type would write the same
// Docker volume. Read-only file mounts are safe to share and are not counted.
func (s *ApplicationService) validateScale(ctx context.Context, appID pgtype.UUID, routable bool, replicas int32) error {
	if replicas < 0 || replicas > maxReplicas {
		return invalid("replicas must be between 0 and %d", maxReplicas)
	}
	if routable && replicas == 0 {
		return invalid("the routable process type cannot be scaled to 0: every domain of this application routes to it, and with no instance the proxy would hold the routes while nothing serves them")
	}
	if replicas > 1 {
		vols, err := s.queries.ListApplicationVolumes(ctx, appID)
		if err != nil {
			return fmt.Errorf("listing volumes: %w", err)
		}
		if len(vols) > 0 {
			return invalid("this application has persistent volumes, and volumes are mounted into every process type, so more than one container would write the same volume; remove the volumes or keep replicas at 1")
		}
	}
	return nil
}

func validateLimits(cpu *float64, memory *int64) error {
	if cpu != nil && (*cpu < 0 || *cpu > 1024) {
		return invalid("cpu_limit must be between 0 and 1024 cores (0 inherits the application's)")
	}
	if memory != nil && *memory != 0 && *memory < minMemoryLimit {
		return invalid("memory_limit must be at least %d bytes (0 inherits the application's)", minMemoryLimit)
	}
	return nil
}

// optFloat / optInt turn an explicit 0 into NULL ("inherit").
func optFloat(v *float64) pgtype.Float8 {
	if v == nil || *v == 0 {
		return pgtype.Float8{}
	}
	return pgtype.Float8{Float64: *v, Valid: true}
}

func optInt(v *int64) pgtype.Int8 {
	if v == nil || *v == 0 {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

// newGeneration returns the 8-hex token that suffixes a freshly minted container name.
//
// It is random rather than a deployment id because the rows are minted here, in the
// API, before any deployment exists (a new type) or for a scale-up that is not a
// deploy; deployment_id on the row is written by the worker when the container is
// created, and the name is an opaque handle (see package instances). Random also keeps
// 3 -> 1 -> 3 from re-minting the name of a surplus container whose removal is still
// in flight, which a "latest deployment" token would.
func newGeneration() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err)) // never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// instanceName mints a container name: {appSlug}-{processType}-{idx}-{generation}.
//
// The suffix is a random token, NOT the deployment id that migration 000079's comment
// describes: rows are minted outside any deploy (a new type, a scale-up), and a
// deployment-derived token can collide with a container still inside its 30s stop
// grace (3 -> 1 -> 3). Nothing may parse a container name, so it only has to be unique.
//
// ⛔ Only for NEW rows. A stored container_name is data and is never recomputed: the
// install still has a routable container named after the pre-0.3.0 slug beside rows
// minted later, and a name derived from parts would point at a container that does
// not exist. The suffix lets two generations of one idx coexist during a roll.
func instanceName(appSlug, typeName string, idx int32, generation string) (string, error) {
	n := fmt.Sprintf("%s-%s-%d-%s", appSlug, typeName, idx, generation)
	if len(n) > dockerNameLimit {
		return "", invalid("the container name %q would be %d characters, over Docker's %d limit; use a shorter process type name or application name", n, len(n), dockerNameLimit)
	}
	return n, nil
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
func (s *ApplicationService) CreateProcessType(ctx context.Context, appID pgtype.UUID, in ProcessTypeInput) (generated.ApplicationProcessType, []generated.ApplicationInstance, ScalePlan, error) {
	if !processTypeNameRe.MatchString(in.Name) {
		return generated.ApplicationProcessType{}, nil, ScalePlan{}, invalid("name must start with a lowercase letter and contain only lowercase letters, digits and '-' (at most 20 characters)")
	}
	if err := validateCommand(in.Command); err != nil {
		return generated.ApplicationProcessType{}, nil, ScalePlan{}, err
	}
	app, err := s.queries.GetApplication(ctx, appID)
	if err != nil {
		return generated.ApplicationProcessType{}, nil, ScalePlan{}, fmt.Errorf("loading application: %w", err)
	}
	if err := validateHealth(app, in.Routable, in.HealthCheckType, in.HealthCheckCommand); err != nil {
		return generated.ApplicationProcessType{}, nil, ScalePlan{}, err
	}

	replicas := int32(1)
	if in.Replicas != nil {
		replicas = *in.Replicas
	}
	if err := s.validateScale(ctx, appID, in.Routable, replicas); err != nil {
		return generated.ApplicationProcessType{}, nil, ScalePlan{}, err
	}
	if err := validateLimits(in.CPULimit, in.MemoryLimit); err != nil {
		return generated.ApplicationProcessType{}, nil, ScalePlan{}, err
	}
	// Checked for the highest idx: a longer idx makes the longer name.
	names := make([]string, 0, replicas)
	generation := newGeneration()
	for idx := int32(1); idx <= replicas; idx++ {
		n, err := instanceName(app.Slug, in.Name, idx, generation)
		if err != nil {
			return generated.ApplicationProcessType{}, nil, ScalePlan{}, err
		}
		names = append(names, n)
	}

	var pt generated.ApplicationProcessType
	var insts []generated.ApplicationInstance
	err = store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		var err error
		pt, err = q.CreateProcessType(ctx, generated.CreateProcessTypeParams{
			ApplicationID:      appID,
			Name:               in.Name,
			Command:            in.Command,
			Replicas:           replicas,
			Routable:           in.Routable,
			CpuLimit:           optFloat(in.CPULimit),
			MemoryLimit:        optInt(in.MemoryLimit),
			HealthCheckType:    optText(in.HealthCheckType),
			HealthCheckCommand: optText(in.HealthCheckCommand),
		})
		if err != nil {
			return err
		}
		for i, n := range names {
			inst, err := q.CreateInstance(ctx, generated.CreateInstanceParams{
				ApplicationID: appID, ProcessTypeID: pt.ID, Idx: int32(i + 1), ContainerName: n, Status: "pending",
			})
			if err != nil {
				return err
			}
			insts = append(insts, inst)
		}
		return nil
	})
	if err != nil {
		return generated.ApplicationProcessType{}, nil, ScalePlan{}, mapProcessTypeUniqueViolation(err)
	}
	s.MarkConfigChanged(ctx, appID)
	// Nothing to do to any container: a new type's containers appear on the next
	// deploy, as they always have. Only SCALING an existing type acts at once.
	return pt, insts, ScalePlan{}, nil
}

// UpdateProcessType applies a patch.
//
// ⛔ UpdateProcessType (the SQL) is a FULL-ROW update, so this reads the stored row
// first and falls back to it for EVERY column the statement writes: command, replicas,
// routable, cpu_limit, memory_limit, health_check_type and health_check_command. A
// partial body passed straight through would silently clear the command, flip
// routable to false, null both health columns and reset the scale and the limits.
//
// A changed replicas count also moves the instance ROWS, in the same transaction:
// surplus rows (idx above the new count) are deleted and missing ones minted, pending.
// The returned plan says what must happen to the containers.
func (s *ApplicationService) UpdateProcessType(ctx context.Context, appID, typeID pgtype.UUID, p ProcessTypePatch) (generated.ApplicationProcessType, []generated.ApplicationInstance, ScalePlan, error) {
	none := ScalePlan{}
	cur, err := s.queries.GetProcessType(ctx, typeID)
	if err != nil || cur.ApplicationID != appID {
		return generated.ApplicationProcessType{}, nil, none, ErrProcessTypeNotFound
	}
	app, err := s.queries.GetApplication(ctx, appID)
	if err != nil {
		return generated.ApplicationProcessType{}, nil, none, fmt.Errorf("loading application: %w", err)
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
	replicas := cur.Replicas
	if p.Replicas != nil {
		replicas = *p.Replicas
	}
	cpu, mem := cur.CpuLimit, cur.MemoryLimit
	if p.CPULimit != nil {
		cpu = optFloat(p.CPULimit)
	}
	if p.MemoryLimit != nil {
		mem = optInt(p.MemoryLimit)
	}

	if err := validateCommand(command); err != nil {
		return generated.ApplicationProcessType{}, nil, none, err
	}
	if err := validateHealth(app, cur.Routable, healthType.String, healthCommand.String); err != nil {
		return generated.ApplicationProcessType{}, nil, none, err
	}
	if err := validateLimits(p.CPULimit, p.MemoryLimit); err != nil {
		return generated.ApplicationProcessType{}, nil, none, err
	}
	if p.Replicas != nil {
		if err := s.validateScale(ctx, appID, cur.Routable, replicas); err != nil {
			return generated.ApplicationProcessType{}, nil, none, err
		}
	}

	plan := ScalePlan{ApplicationID: appID, ProcessTypeID: typeID}
	var pt generated.ApplicationProcessType
	err = store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		var err error
		pt, err = q.UpdateProcessType(ctx, generated.UpdateProcessTypeParams{
			ID:                 typeID,
			Command:            command,
			Replicas:           replicas,
			Routable:           cur.Routable,
			CpuLimit:           cpu,
			MemoryLimit:        mem,
			HealthCheckType:    healthType,
			HealthCheckCommand: healthCommand,
		})
		if err != nil {
			return err
		}
		if replicas == cur.Replicas {
			return nil
		}
		rows, err := q.ListInstancesByProcessType(ctx, typeID)
		if err != nil {
			return fmt.Errorf("listing instances: %w", err)
		}
		have := make(map[int32]bool, len(rows))
		for _, r := range rows {
			if r.Idx > replicas {
				if err := q.DeleteInstance(ctx, r.ID); err != nil {
					return fmt.Errorf("deleting instance %d: %w", r.Idx, err)
				}
				plan.RemoveContainers = append(plan.RemoveContainers, r.ContainerName)
				continue
			}
			have[r.Idx] = true
		}
		generation := newGeneration()
		for idx := int32(1); idx <= replicas; idx++ {
			if have[idx] {
				continue
			}
			name, err := instanceName(app.Slug, cur.Name, idx, generation)
			if err != nil {
				return err
			}
			if _, err := q.CreateInstance(ctx, generated.CreateInstanceParams{
				ApplicationID: appID, ProcessTypeID: typeID, Idx: idx, ContainerName: name, Status: "pending",
			}); err != nil {
				return fmt.Errorf("creating instance %d: %w", idx, err)
			}
			plan.CreateContainers = true
		}
		return nil
	})
	if err != nil {
		var verr *ProcessTypeValidationError
		if errors.As(err, &verr) {
			return generated.ApplicationProcessType{}, nil, none, err
		}
		return generated.ApplicationProcessType{}, nil, none, mapProcessTypeUniqueViolation(err)
	}
	insts, err := s.queries.ListInstancesByProcessType(ctx, typeID)
	if err != nil {
		return generated.ApplicationProcessType{}, nil, none, fmt.Errorf("listing instances: %w", err)
	}
	s.MarkConfigChanged(ctx, appID)
	return pt, insts, plan, nil
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

	// ⛔ The stop and the removal run on a context DETACHED from the request. The
	// request is under the 15s handler timeout, and a stop is slow whenever the command
	// ignores SIGTERM (a pipeline or && chain leaves the shell as PID 1, so Docker waits
	// out its grace period and then kills it). The timeout cancelled the request
	// mid-stop, and the removal that followed ran on the dead context and silently did
	// nothing: the container was left behind as Exited (137) with its rows already gone.
	bg := context.WithoutCancel(ctx)
	rt, rtErr := RuntimeForApplication(bg, s.queries, s.runtimes, appID)
	if rtErr != nil {
		slog.Warn("delete process type: could not reach the application's server", "process_type", cur.Name, "error", rtErr)
		return "process type deleted, but its container could not be removed yet (its server was unreachable); it will be reclaimed by the orphan sweep", nil
	}
	names := make([]string, 0, len(insts))
	for _, in := range insts {
		names = append(names, in.ContainerName)
	}
	// Bounded and concurrent: see removeContainersBounded.
	if failed := removeContainersBounded(bg, rt, names, "delete process type"); len(failed) > 0 {
		warning = "process type deleted, but its container could not be removed yet; it will be reclaimed by the orphan sweep"
	}
	return warning, nil
}

// The two together stay under the 15s handler timeout, so a client is not told the
// request timed out for work that succeeded. RemoveContainer forces (SIGKILL), so it
// does not wait out a grace period of its own.
const (
	processTypeStopGrace     = 5 * time.Second
	processTypeRemoveTimeout = 8 * time.Second
)

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

// ErrInstanceNotFound means no such instance on this application.
var ErrInstanceNotFound = errors.New("instance not found")

// The stop and start each get their own deadline, both detached from the request.
//
// ⛔ instanceStopTimeout must stay ABOVE Docker's stop grace, which StopContainer
// hardcodes at 30s (SIGTERM, wait 30s, SIGKILL). Below it, a worker that ignores
// SIGTERM -- the compound command or pipeline this feature exists for -- outlives
// its own stop context, the stop returns a context error, and the container is left
// down. Delete uses a 5s stop on purpose, because it FORCES removal afterwards and
// needs no grace; restart has no forcing step (there is no kill on the runtime
// interface), so its only lever is to outwait the grace. Do not "align" the two.
// Nothing is waiting on it: the handler answers 202 after 10s.
const (
	instanceStopTimeout  = 40 * time.Second
	instanceStartTimeout = 15 * time.Second
)

// ErrInstanceContainerGone means the instance's container no longer exists (reaped
// or removed), so there is nothing to restart: the answer is a redeploy.
var ErrInstanceContainerGone = errors.New("the container no longer exists; redeploy to recreate it")

// RestartInstance stops and starts ONE container, leaving the application's other
// containers and its status alone. The application-level Restart acts on every
// container and is untouched.
//
// ⛔ Both steps run on a context DETACHED from the request. This is the trap
// DeleteProcessType fell into: the request is under the 15s handler timeout, a stop
// outlives it whenever the command ignores SIGTERM (a compound command leaves the
// shell as PID 1, so Docker waits out its grace and then kills), and a start issued on
// the cancelled request context silently does nothing -- the container stays down.
// The caller decides how long to WAIT; this always runs to completion.
func (s *ApplicationService) RestartInstance(ctx context.Context, appID, instanceID pgtype.UUID) (generated.ApplicationInstance, error) {
	inst, err := s.queries.GetInstance(ctx, instanceID)
	if err != nil || inst.ApplicationID != appID {
		return generated.ApplicationInstance{}, ErrInstanceNotFound
	}

	bg := context.WithoutCancel(ctx)
	rt, err := RuntimeForApplication(bg, s.queries, s.runtimes, appID)
	if err != nil {
		return generated.ApplicationInstance{}, fmt.Errorf("%w: %v", ErrServerUnreachable, err)
	}

	// ⛔ The start is ALWAYS attempted, whatever the stop did. The invariant is that the
	// container ends up running, so a stop that failed or timed out must not skip it
	// (DeleteProcessType learned the same: a failed stop does not skip the removal).
	// Starting a running container is a no-op at the Docker API, so this is safe in
	// every branch. The stop's failure is reported only if the start fails too.
	stopCtx, cancelStop := context.WithTimeout(bg, instanceStopTimeout)
	stopErr := rt.StopContainer(stopCtx, inst.ContainerName)
	cancelStop()
	if stopErr != nil {
		slog.Warn("restart instance: stop did not complete cleanly (starting anyway)", "container", inst.ContainerName, "error", stopErr)
	}

	startCtx, cancelStart := context.WithTimeout(bg, instanceStartTimeout)
	defer cancelStart()
	if err := rt.StartContainer(startCtx, inst.ContainerName); err != nil {
		if exists, xerr := rt.ContainerExists(bg, inst.ContainerName); xerr == nil && !exists {
			return generated.ApplicationInstance{}, ErrInstanceContainerGone
		}
		if stopErr != nil {
			return generated.ApplicationInstance{}, fmt.Errorf("%w: stopping: %v; starting: %v", ErrContainerAction, stopErr, err)
		}
		return generated.ApplicationInstance{}, fmt.Errorf("%w: starting: %v", ErrContainerAction, err)
	}
	return inst, nil
}
