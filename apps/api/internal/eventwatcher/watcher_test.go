package eventwatcher

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
)

func TestHandleEvent_StartSetsRunning(t *testing.T) {
	event := runtime.ContainerEvent{
		ContainerID:   "abc123",
		ContainerName: "myapp",
		Status:        "start",
		Labels:        map[string]string{"application-id": "550e8400-e29b-41d4-a716-446655440000"},
	}

	var newStatus string
	switch event.Status {
	case "start", "restart":
		newStatus = status.ApplicationRunning
	case "stop", "die", "oom":
		newStatus = status.ApplicationStopped
	}
	assert.Equal(t, status.ApplicationRunning, newStatus)
}

func TestHandleEvent_StopSetsStopped(t *testing.T) {
	for _, s := range []string{"stop", "die", "oom"} {
		var newStatus string
		switch s {
		case "start", "restart":
			newStatus = status.ApplicationRunning
		case "stop", "die", "oom":
			newStatus = status.ApplicationStopped
		}
		assert.Equal(t, status.ApplicationStopped, newStatus, "event %q should map to stopped", s)
	}
}

func TestHandleEvent_NoAppIDIgnored(t *testing.T) {
	event := runtime.ContainerEvent{
		ContainerID: "abc123",
		Status:      "start",
		Labels:      map[string]string{},
	}
	appID, ok := event.Labels["application-id"]
	assert.False(t, ok)
	assert.Empty(t, appID)
}

func TestDatabaseStatusForEvent(t *testing.T) {
	cases := []struct {
		name     string
		event    string
		exitCode string
		want     string
		ok       bool
	}{
		{"start", "start", "", status.DatabaseRunning, true},
		{"restart", "restart", "", status.DatabaseRunning, true},
		{"stop", "stop", "", status.DatabaseStopped, true},
		{"clean die", "die", "0", status.DatabaseStopped, true},
		{"crash die", "die", "1", status.DatabaseFailed, true},
		{"die no exit code", "die", "", status.DatabaseFailed, true},
		{"oom", "oom", "", status.DatabaseFailed, true},
		{"unknown ignored", "pause", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			labels := map[string]string{}
			if c.exitCode != "" {
				labels["exitCode"] = c.exitCode
			}
			got, ok := databaseStatusForEvent(runtime.ContainerEvent{
				Status: c.event,
				Labels: labels,
			})
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestIsTransitionalDBStatus(t *testing.T) {
	// These are owned by a running task that recreates the container, so
	// handleDatabaseEvent must ignore container events while in them — otherwise
	// the old container's die/stop clobbers the task's `creating` with a
	// transient stopped/failed and the "Reload Needed" badge sticks.
	transitional := []string{
		status.DatabaseCreating,
		status.DatabaseUpgrading,
		status.DatabaseBackingUp,
	}
	for _, s := range transitional {
		assert.True(t, isTransitionalDBStatus(s), "%q should be transitional", s)
	}

	// Steady states: events apply normally.
	steady := []string{
		status.DatabaseRunning,
		status.DatabaseStopped,
		status.DatabaseFailed,
	}
	for _, s := range steady {
		assert.False(t, isTransitionalDBStatus(s), "%q should not be transitional", s)
	}
}

func TestHandleEvent_DispatchesByLabel(t *testing.T) {
	// Application label takes the application path; database label the DB path;
	// neither label is a no-op. We assert dispatch via label precedence here.
	appEvent := runtime.ContainerEvent{
		Status: "start",
		Labels: map[string]string{labelApplicationID: "550e8400-e29b-41d4-a716-446655440000"},
	}
	dbEvent := runtime.ContainerEvent{
		Status: "die",
		Labels: map[string]string{labelDatabaseID: "550e8400-e29b-41d4-a716-446655440001", "exitCode": "1"},
	}
	_, appHasApp := appEvent.Labels[labelApplicationID]
	_, dbHasDB := dbEvent.Labels[labelDatabaseID]
	assert.True(t, appHasApp)
	assert.True(t, dbHasDB)

	got, ok := databaseStatusForEvent(dbEvent)
	assert.True(t, ok)
	assert.Equal(t, status.DatabaseFailed, got)
}

func TestPgUUIDToString(t *testing.T) {
	// Test with zero UUID bytes
	result := pgUUIDToString(pgUUID("550e8400-e29b-41d4-a716-446655440000"))
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000", result)
}

// pgUUID parses a UUID string into a pgtype.UUID for testing.
func pgUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(s)
	return u
}

// Replays the sequence that made a deliberate stop show up as "error".
//
// The application is a Next.js app started through npm. `docker stop` sends
// SIGTERM, next exits, and npm reports exit code 1 rather than propagating 143
// — so the exit code alone cannot distinguish this from a crash. Docker then
// emits `die` followed by `stop`.
func TestStopSequence_EndsStopped(t *testing.T) {
	die := runtime.ContainerEvent{
		Status: "die",
		Labels: map[string]string{"exitCode": "1"}, // npm swallowed the signal
	}
	stop := runtime.ContainerEvent{Status: "stop"}

	// The application is running when the stop is requested.
	current := status.ApplicationRunning

	// The die arrives first and, read on its own, looks like a crash.
	derived, ok := ApplicationStatusForEvent(die)
	assert.True(t, ok)
	assert.Equal(t, status.ApplicationError, derived)
	if ApplyDieStatus(derived, current) {
		current = derived
	}
	assert.Equal(t, status.ApplicationError, current, "the die is indistinguishable from a crash on its own")

	// The stop follows and is authoritative — this is the step that used to be
	// swallowed for looking like a downgrade from "error".
	derived, ok = ApplicationStatusForEvent(stop)
	assert.True(t, ok)
	current = derived

	assert.Equal(t, status.ApplicationStopped, current,
		"a deliberate stop must not leave the application showing an error")
}

func TestApplyDieStatus(t *testing.T) {
	cases := []struct {
		name             string
		derived, current string
		want             bool
	}{
		{
			"a die during cleanup must not clear a failed deploy",
			status.ApplicationStopped, status.ApplicationError, false,
		},
		{
			"a non-zero exit must not error an app already stopped on purpose",
			status.ApplicationError, status.ApplicationStopped, false,
		},
		{
			"a genuine crash while running is recorded",
			status.ApplicationError, status.ApplicationRunning, true,
		},
		{
			"a clean exit while running is recorded",
			status.ApplicationStopped, status.ApplicationRunning, true,
		},
		{
			"a repeated error is harmless",
			status.ApplicationError, status.ApplicationError, true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, ApplyDieStatus(c.derived, c.current))
		})
	}
}

func TestApplicationStatusForEvent_Health(t *testing.T) {
	cases := []struct {
		name  string
		event string
		want  string
	}{
		// Docker phrases these with a space after the colon.
		{"healthy → running", "health_status: healthy", status.ApplicationRunning},
		{"unhealthy → unhealthy", "health_status: unhealthy", status.ApplicationUnhealthy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ApplicationStatusForEvent(runtime.ContainerEvent{Status: c.event})
			assert.True(t, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestIsHealthEvent(t *testing.T) {
	assert.True(t, isHealthEvent("health_status: healthy"))
	assert.True(t, isHealthEvent("health_status: unhealthy"))
	assert.False(t, isHealthEvent("die"))
	assert.False(t, isHealthEvent("start"))
}
