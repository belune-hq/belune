package handler_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// "Needs attention" counts what is still broken, not every failure in a window.
// The property that matters is that it self-clears: a failed deploy followed by
// a successful one is resolved and must stop being counted, while the 7-day
// deploy-success statistic keeps counting both.
func TestNeedsAttention_FailedDeploysAreResolvedBySuccess(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Attention Project", "attention-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "Attention App", "type": "image", "build_type": "image",
		"source_image": "nginx:latest",
	})
	appID := extractID(app["id"])
	var appUUID pgtype.UUID
	require.NoError(t, appUUID.Scan(appID))

	ctx := context.Background()
	deploy := func(st string) {
		d, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
			ApplicationID: appUUID, Status: status.DeploymentPending, TriggeredBy: "manual",
		})
		require.NoError(t, err)
		_, err = env.Queries.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{
			ID: d.ID, Status: st,
		})
		require.NoError(t, err)
	}

	attention := func() map[string]any {
		resp := env.DoRequest(t, "GET", "/api/stats", nil, testutil.AuthHeader(adminToken))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return testutil.ReadJSON(t, resp)["needs_attention"].(map[string]any)
	}

	// A failure with nothing after it is outstanding.
	deploy(status.DeploymentFailed)
	assert.EqualValues(t, 1, attention()["failed_deploys"],
		"an unfixed failed deploy should need attention")

	// A later success resolves it — this is the whole point of the change.
	deploy(status.DeploymentSuccess)
	assert.EqualValues(t, 0, attention()["failed_deploys"],
		"a failure followed by a successful deploy is resolved")

	// The 7-day statistic is unaffected: it still reports both deploys.
	resp := env.DoRequest(t, "GET", "/api/stats", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	deploy7d := testutil.ReadJSON(t, resp)["deploy_7d"].(map[string]any)
	assert.EqualValues(t, 1, deploy7d["failed"],
		"the success-rate statistic stays historical")
	assert.EqualValues(t, 2, deploy7d["total"])

	// Regressing again re-raises it.
	deploy(status.DeploymentFailed)
	assert.EqualValues(t, 1, attention()["failed_deploys"],
		"a new failure after a success needs attention again")

	// A failed deploy usually also flags the application errored. That is one
	// broken app, so it must be one issue: the deploy bucket yields to the
	// errored bucket rather than both counting the same incident.
	_, err := env.Queries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{
		ID: appUUID, Status: status.ApplicationError,
	})
	require.NoError(t, err)

	na := attention()
	assert.EqualValues(t, 1, na["error_services"], "the app is errored")
	assert.EqualValues(t, 0, na["failed_deploys"],
		"an errored app must not also be counted as a failed deploy")
	assert.EqualValues(t, 1, na["total"],
		"one broken application is one issue, not two")

	// An unhealthy app is up but failing its check. It needs attention in its
	// own right, and — like the errored case — must not be double counted.
	_, err = env.Queries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{
		ID: appUUID, Status: status.ApplicationUnhealthy,
	})
	require.NoError(t, err)

	na = attention()
	assert.EqualValues(t, 1, na["unhealthy_services"],
		"an unhealthy service needs attention")
	assert.EqualValues(t, 0, na["error_services"], "it is not errored")
	assert.EqualValues(t, 0, na["failed_deploys"],
		"an unhealthy app must not also be counted as a failed deploy")
	assert.EqualValues(t, 1, na["total"], "still one issue")
}

// Scheduled backups run on a cron and fail silently, leaving you believing you
// have a backup you do not. Like deploys, the count is per-config and resolves
// when the next run succeeds.
func TestNeedsAttention_ScheduledBackupFailures(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Backup Project", "backup-project")
	projectID := extractID(project["id"])

	ctx := context.Background()
	// Seed a database, a destination and an enabled backup schedule directly:
	// the point under test is the aggregate, not the provisioning flow.
	var dbID, destID, cfgID string
	require.NoError(t, env.Pool.QueryRow(ctx, `
		INSERT INTO databases (project_id, type, name, slug, version, status, credentials_encrypted)
		VALUES ($1::uuid, 'postgres', 'db', 'backup-project-db', '16', 'running', '\x00')
		RETURNING id::text`, projectID).Scan(&dbID))
	require.NoError(t, env.Pool.QueryRow(ctx, `
		INSERT INTO backup_destinations (project_id, name, provider, endpoint, region, bucket, use_ssl, credentials_encrypted)
		VALUES ($1::uuid, 'dest', 's3', 'https://s3.example.com', 'us-east-1', 'b', true, '\x00')
		RETURNING id::text`, projectID).Scan(&destID))
	require.NoError(t, env.Pool.QueryRow(ctx, `
		INSERT INTO database_backup_configs (database_id, destination_id, prefix, schedule, enabled)
		VALUES ($1::uuid, $2::uuid, 'p', '0 3 * * *', true)
		RETURNING id::text`, dbID, destID).Scan(&cfgID))

	attention := func() map[string]any {
		resp := env.DoRequest(t, "GET", "/api/stats", nil, testutil.AuthHeader(adminToken))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return testutil.ReadJSON(t, resp)["needs_attention"].(map[string]any)
	}
	run := func(st string, minutesAgo int) {
		_, err := env.Pool.Exec(ctx, `
			INSERT INTO database_backups (database_id, backup_config_id, status, started_at, target_database)
			VALUES ($1::uuid, $2::uuid, $3, now() - make_interval(mins => $4), 'db')`,
			dbID, cfgID, st, minutesAgo)
		require.NoError(t, err)
	}

	assert.EqualValues(t, 0, attention()["failed_backups"], "no runs yet")

	run("failed", 10)
	assert.EqualValues(t, 1, attention()["failed_backups"],
		"a schedule whose latest run failed needs attention")

	// A later success resolves it, exactly as a successful redeploy does.
	run("succeeded", 5)
	assert.EqualValues(t, 0, attention()["failed_backups"],
		"a failure followed by a successful run is resolved")

	// Disabling the schedule after a failure is not outstanding work.
	run("failed", 1)
	require.EqualValues(t, 1, attention()["failed_backups"])
	_, err := env.Pool.Exec(ctx, `UPDATE database_backup_configs SET enabled = false WHERE id = $1::uuid`, cfgID)
	require.NoError(t, err)
	assert.EqualValues(t, 0, attention()["failed_backups"],
		"a disabled schedule is not outstanding work")
}
