package service

import (
	"errors"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

// HealthCheckInput is the health-check configuration as a caller supplies it,
// independent of transport. The REST handler decodes its own JSON shape into
// this and the MCP tool builds it from tool arguments, so both reach
// ValidateHealthCheck and cannot disagree about what a valid configuration is
// — the same reason EnvKeyRegex lives here rather than in the handler.
type HealthCheckInput struct {
	Type string

	// http
	Path         string
	ExpectStatus int

	// command
	Command            string
	IntervalSeconds    int
	RetriesCount       int
	StartPeriodSeconds int

	// shared
	TimeoutSeconds int
}

// ValidateHealthCheck turns a caller's input into the write parameters, or
// reports why it cannot.
//
// ⚠️ It returns the params for the WHOLE configuration, deliberately leaving
// the unselected mechanism's columns zero so the write clears them. The query
// is one unit for that reason: a row that kept a stale command after switching
// to http would have a later deploy apply a check the operator can no longer
// see. Callers must not cherry-pick fields out of the result.
func ValidateHealthCheck(id pgtype.UUID, in HealthCheckInput) (generated.SetApplicationHealthCheckParams, error) {
	p := generated.SetApplicationHealthCheckParams{ID: id, HealthCheckType: in.Type}

	switch in.Type {
	case "none":
		// Everything stays null.
		return p, nil

	case "http":
		if in.Path == "" {
			return p, errors.New("an HTTP health check needs a path")
		}
		if in.Path[0] != '/' {
			return p, errors.New("the health check path must start with /")
		}
		if in.ExpectStatus != 0 && (in.ExpectStatus < 100 || in.ExpectStatus > 599) {
			return p, errors.New("expected status must be a valid HTTP status code")
		}
		p.HealthCheckPath = nullHealthText(in.Path)
		p.HealthCheckExpectStatus = nullHealthInt4(in.ExpectStatus)
		p.HealthCheckTimeoutSeconds = nullHealthInt4(in.TimeoutSeconds)
		return p, nil

	case "command":
		if in.Command == "" {
			return p, errors.New("a command health check needs a command")
		}
		p.HealthCheckCommand = nullHealthText(in.Command)
		p.HealthCheckIntervalSeconds = nullHealthInt4(in.IntervalSeconds)
		p.HealthCheckRetries = nullHealthInt4(in.RetriesCount)
		p.HealthCheckStartPeriodSeconds = nullHealthInt4(in.StartPeriodSeconds)
		p.HealthCheckTimeoutSeconds = nullHealthInt4(in.TimeoutSeconds)
		return p, nil

	default:
		return p, errors.New("type must be one of: none, http, command")
	}
}

// Zero or less means "unset" for every health-check tuning field — a 0s
// interval, 0 retries or a negative timeout is meaningless, so it maps to NULL
// and the deploy path falls back to its own defaults.
//
// ⚠️ The condition is v > 0, not v != 0. An earlier version of this used
// v != 0 and so persisted a negative value where the handler's own helper had
// turned it into NULL — behaviour drift inside a commit that was presented as
// a pure move. Nothing downstream breaks (every consumer in the deploy path
// guards > 0), but the API response and the dashboard form then showed the
// negative back.
func nullHealthInt4(v int) pgtype.Int4 {
	if v <= 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}
}

func nullHealthText(v string) pgtype.Text {
	return pgtype.Text{String: v, Valid: v != ""}
}
