package handler_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestUpdateApplicationWebhook(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name":        "Webhook App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	secret := "my-webhook-secret"
	branch := "develop"
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/webhook", projectID, appID), map[string]any{
		"webhook_secret":     secret,
		"auto_deploy_branch": branch,
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, branch, result["auto_deploy_branch"])
	// The secret is reported as present but never returned — it used to ride
	// along in this response and in every application fetch.
	assert.Equal(t, true, result["has_webhook_secret"])
	assert.NotContains(t, result, "webhook_secret")

	// It is stored encrypted, not in the legacy plaintext column.
	var hasEncrypted, hasPlaintext bool
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT webhook_secret_encrypted IS NOT NULL, webhook_secret IS NOT NULL
		   FROM applications WHERE id = $1`, appID).Scan(&hasEncrypted, &hasPlaintext))
	assert.True(t, hasEncrypted, "secret must be stored encrypted")
	assert.False(t, hasPlaintext, "plaintext column must be cleared")

	// Reveal is the only way back to the value.
	resp = env.DoRequest(t, "GET",
		fmt.Sprintf("/api/projects/%s/applications/%s/webhook/reveal", projectID, appID),
		nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, secret, testutil.ReadJSON(t, resp)["webhook_secret"])
}

// No application-shaped response may carry a secret column. This is the
// regression guard for the leak the DTO was introduced to close.
func TestApplicationResponse_OmitsSecrets(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Secret App", "type": "git",
		"build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
		"git_token": "ghp_supersecret",
	})
	appID := extractID(app["id"])

	secretKeys := []string{
		"webhook_secret",
		"git_credentials_encrypted",
		"deploy_hook_token_hash",
		"deploy_hook_token_encrypted",
	}

	// Create response.
	for _, k := range secretKeys {
		assert.NotContains(t, app, k, "create response leaked %s", k)
	}
	assert.Equal(t, true, app["has_webhook_secret"])
	assert.Equal(t, true, app["has_git_credentials"])

	// Get response.
	resp := env.DoRequest(t, "GET",
		fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID),
		nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	detail := testutil.ReadJSON(t, resp)
	for _, k := range secretKeys {
		assert.NotContains(t, detail, k, "get response leaked %s", k)
	}

	// List response.
	resp = env.DoRequest(t, "GET",
		fmt.Sprintf("/api/projects/%s/applications", projectID),
		nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	resp.Body.Close()
	require.Len(t, list, 1)
	for _, k := range secretKeys {
		assert.NotContains(t, list[0], k, "list response leaked %s", k)
	}
}

func TestWebhookPush_GitHub(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	// Create app with source_repo and webhook secret
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name":        "Webhook App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	secret := "test-secret-123"
	env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/webhook", projectID, appID), map[string]any{
		"webhook_secret":     secret,
		"auto_deploy_branch": "main",
	}, testutil.AuthHeader(token)).Body.Close()

	// Simulate GitHub push webhook
	payload := map[string]any{
		"ref":   "refs/heads/main",
		"after": "abc123def456",
		"head_commit": map[string]any{
			"id":      "abc123def456",
			"message": "Fix login redirect",
			"author":  map[string]any{"name": "Alice", "username": "alice"},
		},
		"repository": map[string]any{
			"clone_url": "https://github.com/test/repo.git",
		},
	}
	body, _ := json.Marshal(payload)

	// Compute HMAC signature
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	resp := env.DoRequest(t, "POST", "/api/webhooks/push", payload, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": signature,
	})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Verify deploy task was enqueued
	require.Len(t, env.Asynq.Tasks, 1)
	assert.Equal(t, "deploy", env.Asynq.Tasks[0].TypeName)

	// The commit provenance the payload carried must be persisted, not just the
	// SHA — it is what the deployments list renders.
	var sha, message, author string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT commit_sha, COALESCE(commit_message, ''), COALESCE(commit_author, '')
		   FROM deployments WHERE application_id = $1`, appID,
	).Scan(&sha, &message, &author))
	assert.Equal(t, "abc123def456", sha)
	assert.Equal(t, "Fix login redirect", message)
	assert.Equal(t, "Alice", author)
}

func TestWebhookPush_DuplicateDelivery(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name":        "Webhook App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	secret := "test-secret-123"
	env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/webhook", projectID, appID), map[string]any{
		"webhook_secret":     secret,
		"auto_deploy_branch": "main",
	}, testutil.AuthHeader(token)).Body.Close()

	payload := map[string]any{
		"ref":   "refs/heads/main",
		"after": "abc123def456",
		"repository": map[string]any{
			"clone_url": "https://github.com/test/repo.git",
		},
	}
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	headers := map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": signature,
	}

	// First delivery creates a deployment.
	resp := env.DoRequest(t, "POST", "/api/webhooks/push", payload, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.Len(t, env.Asynq.Tasks, 1)

	// Duplicate delivery (same commit SHA) within the idempotency window is
	// deduped — no new deployment, no new enqueue.
	resp = env.DoRequest(t, "POST", "/api/webhooks/push", payload, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	assert.Len(t, env.Asynq.Tasks, 1, "duplicate webhook should not enqueue a second deploy")
}

func TestWebhookPush_BranchMismatch(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name":        "Webhook App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	secret := "test-secret-123"
	env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/webhook", projectID, appID), map[string]any{
		"webhook_secret":     secret,
		"auto_deploy_branch": "main",
	}, testutil.AuthHeader(token)).Body.Close()

	// Push to develop branch (mismatch)
	payload := map[string]any{
		"ref":   "refs/heads/develop",
		"after": "abc123def456",
		"repository": map[string]any{
			"clone_url": "https://github.com/test/repo.git",
		},
	}
	body, _ := json.Marshal(payload)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	resp := env.DoRequest(t, "POST", "/api/webhooks/push", payload, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": signature,
	})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// No deploy should be enqueued
	assert.Len(t, env.Asynq.Tasks, 0)
}
