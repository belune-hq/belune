package notify

// EventDef is the canonical description of a routable event type. The registry
// below is the single source of truth: the API serves it to the frontend so the
// subscription checkboxes cannot drift from these Go constants, and channel
// validation rejects subscriptions to unknown types.
type EventDef struct {
	Type        string   `json:"type"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Severity    Severity `json:"severity"`
	// Group is a UI grouping key for the subscription checkboxes.
	Group string `json:"group"`
}

// Event type constants. Every one of these already fires through the in-app
// notification pipeline (deploy_task, backup_db_task, tls_status_task); this
// release only routes them outward — no new events are introduced here.
const (
	EventDeploymentFailed    = "deployment.failed"
	EventDeploymentSucceeded = "deployment.succeeded"

	EventDatabaseBackupFailed  = "database.backup_failed"
	EventDatabaseRestored      = "database.restored"
	EventDatabaseRestoreFailed = "database.restore_failed"

	EventTLSExpiring = "tls.expiring"
	EventTLSExpired  = "tls.expired"
	EventTLSFailed   = "tls.failed"

	EventVolumeBackupFailed  = "application.volume_backup_failed"
	EventVolumeRestored      = "application.volume_restored"
	EventVolumeRestoreFailed = "application.volume_restore_failed"

	EventScheduledTaskFailed = "application.scheduled_task_failed"

	// EventUpdateAvailable fires from the daily update-check worker
	// (worker/update_check_task.go) the first time a manifest fetch finds a
	// version newer than the one running — a transition, not every tick.
	EventUpdateAvailable = "update.available"
)

// eventRegistry lists every event a channel may subscribe to, in display order.
var eventRegistry = []EventDef{
	{Type: EventDeploymentSucceeded, Label: "Deployment succeeded", Description: "An application finished deploying successfully.", Severity: SeverityOK, Group: "Deployments"},
	{Type: EventDeploymentFailed, Label: "Deployment failed", Description: "An application deployment failed.", Severity: SeverityError, Group: "Deployments"},

	{Type: EventDatabaseBackupFailed, Label: "Backup failed", Description: "A scheduled or manual database backup failed.", Severity: SeverityError, Group: "Databases"},
	{Type: EventDatabaseRestored, Label: "Restore completed", Description: "A database was restored from a backup.", Severity: SeverityOK, Group: "Databases"},
	{Type: EventDatabaseRestoreFailed, Label: "Restore failed", Description: "A database restore failed.", Severity: SeverityError, Group: "Databases"},

	{Type: EventTLSExpiring, Label: "Certificate expiring", Description: "A TLS certificate is approaching expiry.", Severity: SeverityWarn, Group: "TLS"},
	{Type: EventTLSExpired, Label: "Certificate expired", Description: "A TLS certificate has expired.", Severity: SeverityError, Group: "TLS"},
	{Type: EventTLSFailed, Label: "Certificate failed", Description: "A TLS certificate could not be issued or renewed.", Severity: SeverityError, Group: "TLS"},

	{Type: EventVolumeBackupFailed, Label: "Volume backup failed", Description: "An application volume backup failed.", Severity: SeverityError, Group: "Volumes"},
	{Type: EventVolumeRestored, Label: "Volume restored", Description: "An application volume was restored from a backup.", Severity: SeverityOK, Group: "Volumes"},
	{Type: EventVolumeRestoreFailed, Label: "Volume restore failed", Description: "An application volume restore failed.", Severity: SeverityError, Group: "Volumes"},

	{Type: EventScheduledTaskFailed, Label: "Scheduled task failed", Description: "A scheduled task failed or timed out.", Severity: SeverityError, Group: "Scheduled Tasks"},

	{Type: EventUpdateAvailable, Label: "Update available", Description: "A newer Belune release is available.", Severity: SeverityWarn, Group: "Platform"},
}

// Events returns the canonical registry (a copy is unnecessary; callers must not
// mutate the slice).
func Events() []EventDef { return eventRegistry }

// knownEvents indexes the registry for O(1) membership checks.
var knownEvents = func() map[string]struct{} {
	m := make(map[string]struct{}, len(eventRegistry))
	for _, e := range eventRegistry {
		m[e.Type] = struct{}{}
	}
	return m
}()

// IsKnownEvent reports whether eventType is a subscribable event.
func IsKnownEvent(eventType string) bool {
	_, ok := knownEvents[eventType]
	return ok
}

// eventLabels indexes the registry by type for label lookups.
var eventLabels = func() map[string]string {
	m := make(map[string]string, len(eventRegistry))
	for _, e := range eventRegistry {
		m[e.Type] = e.Label
	}
	return m
}()

// EventLabel returns the human-readable label for an event type, or the raw
// type when it isn't in the registry (e.g. an event retired in a later version).
func EventLabel(eventType string) string {
	if label, ok := eventLabels[eventType]; ok {
		return label
	}
	return eventType
}
