package server

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"

	"github.com/weiliang79/belune/internal/handler"
	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/service"
)

const (
	defaultBodyLimit = 1 << 20 // 1 MB — applied to all non-streaming routes
	envBodyLimit     = 5 << 20 // 5 MB — raised for bulk env-var imports
	handlerTimeout   = 15 * time.Second
)

// Requests per minute on the authenticated surface. An interactive session and
// an automated token get SEPARATE budgets, because they have nothing in common
// but the key function — and a single shared number has to be either too small
// for a dashboard or too large for a script.
//
// ⚠️ Measured, 2026-10-09, one Chromium tab against a real production stack,
// counting only what reaches this group, as the peak inside a 60s sliding
// window (which is what httprate computes):
//
//	Application detail      33    Projects (landing)   15
//	Application settings    31    Server → Overview    10
//	Server → Configuration  28    Docker                8
//	Project detail          25    Requests, Certs       7
//
// plus 5–16 for every navigation and 16 for a full reload. rateLimitKey
// resolves a browser session to user:<id>, so EVERY TAB OF EVERY WINDOW SHARES
// ONE BUCKET: three tabs on an application page is ~99/min before the operator
// touches anything. That is how a routine v0.1.16 update drill — one operator,
// a couple of tabs, repeated reloads while watching an update — hit 429 on a
// dashboard doing nothing unusual.
//
// 600 is ~2x the plausible worst case (six heavy tabs plus navigation churn)
// and still only 10 req/s, orders of magnitude below what a runaway client
// does. The token budget stays where it was: a script needing more than 100/min
// is polling, and should be told so rather than quietly granted a human's
// allowance.
//
// ⛔ Do NOT "fix" a future 429 by raising the shared number — there no longer is
// one, and that is the point. Measure which caller is hitting it first:
// scripts/smoke-browser.sh has the drill.
const (
	sessionRateLimit = 600
	tokenRateLimit   = 100
)

// rateLimitByCaller applies tokenRateLimit to a PAT-authenticated request and
// sessionRateLimit to everything else, keeping the two counters separate.
//
// Each caller kind reaches only its own limiter, so a token can never spend a
// session's allowance or the reverse — which rateLimitKey already guaranteed
// for the KEY, and this now guarantees for the SIZE.
func rateLimitByCaller() func(http.Handler) http.Handler {
	limit := func(n int) func(http.Handler) http.Handler {
		return httprate.Limit(n, time.Minute,
			httprate.WithKeyFuncs(rateLimitKey),
			httprate.WithLimitHandler(rateLimitExceeded))
	}
	session, token := limit(sessionRateLimit), limit(tokenRateLimit)
	return func(next http.Handler) http.Handler {
		// Wrapped once here, not per request: each httprate.Limit closes over
		// its own counter, and that state has to outlive a single request to
		// count anything at all.
		sessionNext, tokenNext := session(next), token(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if middleware.TokenIDFromContext(r.Context()) != "" {
				tokenNext.ServeHTTP(w, r)
				return
			}
			sessionNext.ServeHTTP(w, r)
		})
	}
}

// rateLimitExceeded replaces httprate's plain-text http.Error with the API's
// own error shape — see handler.WriteRateLimited. The window is a minute, which
// is also what httprate itself puts in Retry-After.
func rateLimitExceeded(w http.ResponseWriter, _ *http.Request) {
	handler.WriteRateLimited(w,
		"too many requests — wait a minute and retry", time.Minute)
}

// rateLimitKey keys rate limiting by the authenticating PAT's id first, then
// falls back to the session user id, then to IP for unauthenticated requests.
// Token id takes priority over user id so a runaway script on one token
// cannot starve the same user's other tokens (or their human session) —
// separate buckets, per the PAT design.
func rateLimitKey(r *http.Request) (string, error) {
	if tokenID := middleware.TokenIDFromContext(r.Context()); tokenID != "" {
		return "token:" + tokenID, nil
	}
	if userID := middleware.UserIDFromContext(r.Context()); userID != "" {
		return "user:" + userID, nil
	}
	return httprate.KeyByIP(r)
}

// withTimeout wraps a handler in an http.TimeoutHandler so slow-loris attacks
// cannot hold non-streaming connections open indefinitely.
func withTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, `{"error":"request timeout"}`)
	}
}

func registerRoutes(r chi.Router, h *handler.Handler, auth *service.AuthService, tokens *service.TokenService, disableRateLimit bool) {
	// Health check (unauthenticated; no body limit needed, no timeout applied so
	// health-check pollers with long intervals are not artificially rejected).
	r.Get("/healthz", h.HealthCheck)

	// Public routes — small body limit; no timeout on login (rate-limited instead)
	r.Group(func(r chi.Router) {
		r.Use(middleware.BodyLimit(defaultBodyLimit))

		// Login rate limit: 5 req/min per IP
		r.Group(func(r chi.Router) {
			if !disableRateLimit {
				r.Use(httprate.LimitByIP(5, time.Minute))
			}
			r.With(withTimeout(handlerTimeout)).Post("/api/auth/login", h.Login)
		})

		// The second step gets its own bucket rather than sharing the login one.
		// Sharing it charged a normal two-step sign-in two of five requests, so
		// a couple of mistyped codes returned 429 — which the client can only
		// read as the account lockout, telling the user to go and find an admin.
		// Guessing is still bounded: five attempts kill the challenge itself.
		r.Group(func(r chi.Router) {
			if !disableRateLimit {
				r.Use(httprate.LimitByIP(10, time.Minute))
			}
			r.With(withTimeout(handlerTimeout)).Post("/api/auth/login/verify", h.VerifyLogin)
		})

		r.With(withTimeout(handlerTimeout)).Get("/api/auth/setup", h.Setup)
		r.With(withTimeout(handlerTimeout)).Post("/api/auth/setup", h.Setup)
		r.With(withTimeout(handlerTimeout)).Get("/api/features", h.GetFeatures)
		r.With(withTimeout(handlerTimeout)).Get("/api/version", h.GetVersion)

		// Refresh: cookie-driven, no Auth middleware — but CSRF and rate
		// limit still apply. 30 req/min per IP is generous enough for normal
		// SPA usage (one refresh per access expiry) and tight enough that a
		// stolen refresh cookie cannot be brute-rotated against.
		r.Group(func(r chi.Router) {
			r.Use(middleware.CSRF())
			if !disableRateLimit {
				r.Use(httprate.LimitByIP(30, time.Minute))
			}
			r.With(withTimeout(handlerTimeout)).Post("/api/auth/refresh", h.Refresh)
		})

		// Password reset: forgot-password 3/hour by IP; reset-password 10/min by IP.
		r.Group(func(r chi.Router) {
			if !disableRateLimit {
				r.Use(httprate.LimitByIP(3, time.Hour))
			}
			r.With(withTimeout(handlerTimeout)).Post("/api/auth/forgot-password", h.ForgotPassword)
		})
		r.Group(func(r chi.Router) {
			if !disableRateLimit {
				r.Use(httprate.LimitByIP(10, time.Minute))
			}
			r.With(withTimeout(handlerTimeout)).Post("/api/auth/reset-password", h.ResetPassword)
		})

		// Invitation acceptance: peek 30/min by IP; accept 10/min by IP.
		r.Group(func(r chi.Router) {
			if !disableRateLimit {
				r.Use(httprate.LimitByIP(30, time.Minute))
			}
			r.With(withTimeout(handlerTimeout)).Get("/api/auth/invitation", h.GetInvitation)
		})
		r.Group(func(r chi.Router) {
			if !disableRateLimit {
				r.Use(httprate.LimitByIP(10, time.Minute))
			}
			r.With(withTimeout(handlerTimeout)).Post("/api/auth/accept-invitation", h.AcceptInvitation)
		})

		// Webhooks: 30 req/min per IP
		r.Group(func(r chi.Router) {
			if !disableRateLimit {
				r.Use(httprate.LimitByIP(30, time.Minute))
			}
			r.With(withTimeout(handlerTimeout)).Post("/api/webhooks/push", h.HandleWebhookPush)
			// Provider App/OAuth webhooks (verified against the provider app's
			// shared webhook secret).
			r.With(withTimeout(handlerTimeout)).Post("/api/git/webhooks/{provider}", h.HandleProviderWebhook)
			// Per-application deploy hook. Unauthenticated by design: the token
			// in the path is the credential, so CI can fire it with a bare curl.
			r.With(withTimeout(handlerTimeout)).Post("/api/webhooks/deploy/{token}", h.HandleDeployHook)
		})

		// Git provider OAuth/manifest callbacks are public: they are top-level
		// browser redirects from the provider that carry no Authorization header,
		// so they are guarded by a one-time state nonce instead of the JWT.
		r.Group(func(r chi.Router) {
			r.Use(withTimeout(handlerTimeout))
			r.Get("/api/git/providers/github/manifest/callback", h.HandleGitHubAppManifestCallback)
			r.Get("/api/git/integrations/callback", h.HandleGitIntegrationCallback)
		})
	})

	// WebSocket routes: auth-protected, no body limit, no timeout (long-lived connections).
	// httprate must NOT be applied here — it wraps ResponseWriter in a way that
	// breaks Hijacker and prevents the protocol upgrade.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Auth(auth, tokens))
		r.With(middleware.RequireScope("read")).Get("/api/ws", h.HandleWebSocket)
		// Session-only: an interactive shell into a running container is far
		// more than any scope was ever meant to convey, and a PAT attaching to
		// another user's live terminal tunnel is exactly the gap a 2026-09-05
		// review found — Auth() now accepts PATs too, so "the WS group already
		// requires a valid session" (the old assumption here) stopped being
		// true the moment PR2 landed. See also CreateTerminalSession below.
		r.With(middleware.RequireSession()).Get("/api/ws/terminal/{sessionId}", h.HandleTerminalWebSocket)
	})

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(middleware.Auth(auth, tokens))
		r.Use(middleware.CSRF())
		if !disableRateLimit {
			r.Use(rateLimitByCaller())
		}

		// Standard JSON routes: 1 MB body limit + 15 s timeout.
		r.Group(func(r chi.Router) {
			r.Use(middleware.BodyLimit(defaultBodyLimit))
			r.Use(withTimeout(handlerTimeout))

			// Admin-only routes
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireRole("admin"))
				r.Use(middleware.RequireScopeByMethod())

				// User management is credential issuance — an admin-role PAT
				// creating a fresh admin account (with a password it chose)
				// and logging in as it would sail straight through every
				// RequireSession gate below as a real session, making this
				// the same self-propagation class PR3's review closed for
				// /api/tokens itself. ResetUserPassword and AdminResetUserTOTP
				// are worse: they take over an EXISTING admin account with no
				// re-verification at all. All require a session.
				r.Get("/api/users", h.ListUsers)
				r.With(middleware.RequireSession()).Put("/api/users/{userId}/role", h.UpdateUserRole)
				r.With(middleware.RequireSession()).Delete("/api/users/{userId}", h.DeleteUser)

				r.Group(func(r chi.Router) {
					if !disableRateLimit {
						r.Use(httprate.LimitByIP(10, time.Minute))
					}
					r.With(middleware.RequireSession()).Post("/api/users", h.CreateUser)
					r.With(middleware.RequireSession()).Post("/api/users/invite", h.InviteUser)
					r.With(middleware.RequireSession()).Put("/api/users/{userId}/password", h.ResetUserPassword)
					r.With(middleware.RequireSession()).Post("/api/users/{userId}/totp/reset", h.AdminResetUserTOTP)
				})
				r.Get("/api/users/invitations", h.ListPendingInvitations)
				r.Delete("/api/users/invitations/{invitationId}", h.RevokeInvitation)

				// Backup management
				r.Get("/api/backups", h.ListBackupRuns)
				r.Get("/api/backups/status", h.GetBackupStatus)
				r.Post("/api/backups/run", h.TriggerBackupRun)
				r.Post("/api/backups/test", h.TestBackupRemote)
				r.Put("/api/backups/remote", h.UpdateBackupRemote)
				// Tests posted form values instead of the saved config, so the
				// Remote Storage card can validate credentials before they are
				// committed. Writes nothing despite the POST.
				r.Post("/api/backups/remote/test", h.TestBackupRemoteParams)
			})

			// The bulk of the authenticated surface: scope defaults to "read"
			// for a safe method and "write" otherwise, and a project-pinned
			// token is rejected outside its pin. A new route added inside this
			// group needs no scope annotation to be safe by default — routes
			// needing something else (deploy actions, metrics reads, or a
			// session instead of any token) are carved out below, or gated
			// in place with an extra .With(...) on that one line.
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireScopeByMethod())
				r.Use(middleware.RequirePinnedProjectMatch())

				// A PAT manages infrastructure, not the account itself: every
				// route below through the TOTP block now requires a live
				// session, except Me — reporting the token's OWN identity and
				// role is how a script confirms what it authenticated as, not
				// account management. None of this closes an account-takeover
				// path that existed — ChangeOwnPassword bcrypt-verifies
				// current_password, DisableTOTP/RegenerateRecoveryCodes each
				// already required the password AND a current second factor,
				// EnrollTOTP already required the password, and UpdateProfile
				// only ever wrote username/first/last name, never email. This
				// is defense in depth plus removing a recon surface, not a
				// vulnerability fix.
				r.With(middleware.RequireSession()).Post("/api/auth/logout", h.Logout)
				r.Get("/api/auth/me", h.Me)
				r.With(middleware.RequireSession()).Put("/api/auth/password", h.ChangeOwnPassword)
				r.With(middleware.RequireSession()).Put("/api/auth/profile", h.UpdateProfile)

				// GetTOTPStatus is read-only but reports whether MFA is enabled
				// — security posture is recon value on its own, so it's gated
				// with the mutations rather than carved out as PAT-safe.
				r.With(middleware.RequireSession()).Get("/api/auth/totp", h.GetTOTPStatus)
				r.With(middleware.RequireSession()).Post("/api/auth/totp/enroll", h.EnrollTOTP)
				r.With(middleware.RequireSession()).Post("/api/auth/totp/enroll/verify", h.VerifyTOTPEnrollment)
				r.With(middleware.RequireSession()).Post("/api/auth/totp/disable", h.DisableTOTP)
				r.With(middleware.RequireSession()).Post("/api/auth/totp/recovery-codes", h.RegenerateRecoveryCodes)

				// Alert preferences are infrastructure config (deployment
				// failures, resource thresholds — the things a token already
				// manages), not account security, so they were briefly gated
				// above alongside the account-security block and then moved
				// back here: a provisioning script configuring its own alert
				// thresholds is legitimate, gating bought no credential
				// exposure or takeover-path protection, and the only "recon"
				// given up is "this account receives email." Deliberately on
				// the other side of the "PAT manages infrastructure, not the
				// account" line from everything above it.
				r.Get("/api/account/alert-preferences", h.GetAlertPreferences)
				r.Put("/api/account/alert-preferences", h.UpdateAlertPreferences)

				// Personal access tokens: self-service, scoped to the caller. No
				// admin oversight view exists in v1 — see project_v016_plan.
				// Minting and revoking already required a live session — a PAT
				// calling them would be a self-propagation path (mint a longer-
				// lived replacement, revoke the original) scope enforcement
				// alone can't close. Listing now joins them: it returns every
				// token's id, name, scopes, role_at_issue, and last-used/expiry
				// timestamps — masked correctly, but still a credential
				// inventory a leaked low-scope token could use to map out which
				// other tokens exist, which holds write, which hasn't been used
				// in months, and the id DeleteAPIToken takes.
				r.With(middleware.RequireSession()).Get("/api/tokens", h.ListAPITokens)
				r.With(middleware.RequireSession()).Post("/api/tokens", h.CreateAPIToken)
				r.With(middleware.RequireSession()).Delete("/api/tokens/{tokenId}", h.DeleteAPIToken)

				// Git connections (per-user connected provider accounts)
				r.Get("/api/git/integrations", h.ListGitIntegrations)
				r.Get("/api/git/integrations/available", h.ListAvailableProviders)
				r.Get("/api/git/integrations/connect", h.StartGitIntegrationConnect)
				r.Get("/api/git/integrations/{integrationId}/repos", h.ListIntegrationRepos)
				r.Get("/api/git/integrations/{integrationId}/branches", h.ListIntegrationBranches)
				r.Delete("/api/git/integrations/{integrationId}", h.DeleteGitIntegration)

				// App templates (catalog + one-click instantiation)
				r.Get("/api/templates", h.ListTemplates)
				r.Get("/api/templates/{templateId}", h.GetTemplate)
				r.Post("/api/templates/{templateId}/instantiate", h.InstantiateTemplate)

				// Projects
				r.Get("/api/projects", h.ListProjects)
				r.Post("/api/projects", h.CreateProject)
				r.Get("/api/projects/{projectId}", h.GetProject)
				r.Put("/api/projects/{projectId}", h.UpdateProject)
				// Destroying the project itself needs a session — see the
				// destroy-boundary test, which discovers this route (and every
				// other one below carrying RequireSession) mechanically rather
				// than off a hand-maintained list.
				r.With(middleware.RequireSession()).Delete("/api/projects/{projectId}", h.DeleteProject)
				// Both hand out project-owner-equivalent access rather than operate a
				// workload — sharing extends it to every Member (canAccessOwned's
				// `if shared { return true }` has no membership check, so this is
				// what hands every Member RevealEnvVar access on the project), and
				// transfer moves it outright to a named user. Same class as
				// POST /api/users and POST /api/users/invite above: administering
				// who can reach what, not operating what's already reachable.
				// TransferProject's admin-only requirement moves here from the
				// handler body too — enforcing it in routes.go, not a
				// //apidoc:roles declaration, is what lets the generator DERIVE
				// x-belune-roles instead of adding a second, driftable source of
				// truth (see apidocRequireRoleSet in apidoc_generate_test.go).
				r.With(middleware.RequireSession(), middleware.RequireRole("admin")).
					Put("/api/projects/{projectId}/transfer", h.TransferProject)
				r.With(middleware.RequireSession()).Put("/api/projects/{projectId}/sharing", h.UpdateProjectSharing)

				// Applications
				r.Get("/api/projects/{projectId}/applications", h.ListApplications)
				r.Post("/api/projects/{projectId}/applications", h.CreateApplication)
				r.Get("/api/projects/{projectId}/applications/{applicationId}", h.GetApplication)
				r.Put("/api/projects/{projectId}/applications/{applicationId}", h.UpdateApplication)
				r.Put("/api/projects/{projectId}/applications/{applicationId}/runtime", h.UpdateApplicationRuntime)
				r.With(middleware.RequireSession()).Delete("/api/projects/{projectId}/applications/{applicationId}", h.DeleteApplication)
				r.Put("/api/projects/{projectId}/applications/{applicationId}/webhook", h.UpdateApplicationWebhook)
				// Reveal endpoints decrypt a real secret and return it in plaintext
				// (webhook secret, deploy-hook token, file-mount contents, env var
				// value) — "read" scope alone would hand a read-only PAT every
				// secret in the install, since read is derived purely from the
				// HTTP method (RequireScopeByMethod), not from what the GET
				// actually returns. Session-gated instead, same reasoning as the
				// destroy boundary; see reveal_boundary_test.go for the structural
				// enforcement.
				r.With(middleware.RequireSession()).Get("/api/projects/{projectId}/applications/{applicationId}/webhook/reveal", h.RevealWebhookSecret)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/deploy-hook", h.GetDeployHook)
				r.With(middleware.RequireSession()).Get("/api/projects/{projectId}/applications/{applicationId}/deploy-hook/reveal", h.RevealDeployHook)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/deploy-hook", h.GenerateDeployHook)
				r.Delete("/api/projects/{projectId}/applications/{applicationId}/deploy-hook", h.DeleteDeployHook)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/change-source", h.ChangeApplicationSource)
				r.Put("/api/projects/{projectId}/applications/{applicationId}/health-check", h.SetHealthCheck)
				r.Put("/api/projects/{projectId}/applications/{applicationId}/resources", h.SetResources)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/cache", h.GetBuildCache)
				r.Delete("/api/projects/{projectId}/applications/{applicationId}/cache", h.ClearBuildCache)

				// Preview environments: parent config + child list + child delete
				r.Put("/api/projects/{projectId}/applications/{applicationId}/previews/config", h.UpdatePreviewConfig)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/previews", h.ListPreviews)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/previews/{previewId}", h.GetPreview)
				r.Delete("/api/projects/{projectId}/applications/{applicationId}/previews/{previewId}", h.DeletePreview)

				// Deployments
				r.Get("/api/projects/{projectId}/applications/{applicationId}/deployments", h.ListDeployments)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/deployments/{deploymentId}", h.GetDeployment)

				// Latest post-deploy health-probe result
				r.Get("/api/projects/{projectId}/applications/{applicationId}/health", h.GetApplicationHealth)

				// Terminal session creation: session-only, same reasoning as the
				// WS tunnel above (exec is short; the tunnel itself is in the WS
				// group, gated there too).
				r.With(middleware.RequireSession()).Post("/api/projects/{projectId}/applications/{applicationId}/terminal", h.CreateTerminalSession)

				// Application logs history (paginated query, not a stream)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/logs/history", h.ListApplicationLogs)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/logs/sessions", h.ListApplicationLogSessions)

				// Request logs (paginated)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/requests", h.ListRequestLogs)

				// Domains
				r.Get("/api/projects/{projectId}/applications/{applicationId}/domains", h.ListDomains)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/domains/{domainId}", h.GetDomain)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/domains", h.AddDomain)
				r.Put("/api/projects/{projectId}/applications/{applicationId}/domains/{domainId}", h.UpdateDomain)
				r.With(middleware.RequireSession()).Delete("/api/projects/{projectId}/applications/{applicationId}/domains/{domainId}", h.RemoveDomain)

				// Domain route features
				r.Post("/api/projects/{projectId}/applications/{applicationId}/domains/{domainId}/tls/recheck", h.RecheckDomainTLS)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/domains/{domainId}/features", h.ListRouteFeatures)
				r.Put("/api/projects/{projectId}/applications/{applicationId}/domains/{domainId}/features", h.UpsertRouteFeature)
				r.Delete("/api/projects/{projectId}/applications/{applicationId}/domains/{domainId}/features/{featureId}", h.DeleteRouteFeature)

				// Application persistent volumes
				r.Get("/api/projects/{projectId}/applications/{applicationId}/volumes", h.ListApplicationVolumes)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/volumes", h.CreateApplicationVolume)
				r.With(middleware.RequireSession()).Delete("/api/projects/{projectId}/applications/{applicationId}/volumes/{volumeId}", h.DeleteApplicationVolume)

				// Application volume backups
				r.Get("/api/projects/{projectId}/applications/{applicationId}/volumes/{volumeId}/backup-configs", h.ListBackupConfigsForVolume)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/volumes/{volumeId}/backup-configs", h.CreateVolumeBackupConfig)
				r.Put("/api/projects/{projectId}/applications/{applicationId}/volumes/{volumeId}/backup-configs/{configId}", h.UpdateVolumeBackupConfig)
				r.Delete("/api/projects/{projectId}/applications/{applicationId}/volumes/{volumeId}/backup-configs/{configId}", h.DeleteVolumeBackupConfig)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/volumes/{volumeId}/backup-configs/{configId}/run", h.RunVolumeBackupConfig)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/volumes/{volumeId}/backups", h.ListVolumeBackups)
				r.With(middleware.RequireSession()).Post("/api/projects/{projectId}/applications/{applicationId}/volumes/{volumeId}/backups/{backupId}/restore", h.RestoreVolumeBackup)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/volumes/{volumeId}/restores", h.ListVolumeRestores)
				r.Get("/api/projects/{projectId}/applications/{applicationId}/volume-backup-configs", h.ListBackupConfigsForApplication)

				// Application file/config mounts
				r.Get("/api/projects/{projectId}/applications/{applicationId}/file-mounts", h.ListFileMounts)
				r.With(middleware.RequireSession()).Get("/api/projects/{projectId}/applications/{applicationId}/file-mounts/{fileMountId}/reveal", h.RevealFileMount)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/file-mounts", h.CreateFileMount)
				r.Put("/api/projects/{projectId}/applications/{applicationId}/file-mounts/{fileMountId}", h.UpdateFileMount)
				r.Delete("/api/projects/{projectId}/applications/{applicationId}/file-mounts/{fileMountId}", h.DeleteFileMount)

				// Global deployments
				r.Get("/api/deployments", h.GetGlobalDeployments)

				// Which certificate can serve a hostname. Member-reachable
				// because attaching one is project-scoped and therefore theirs
				// to do; the full certificate list stays admin-only, since it
				// carries every SAN of every certificate on the install.
				r.Get("/api/certificates/usable", h.ListUsableCertificates)

				// Every domain's observed TLS state in one view. Role-scoped
				// rather than admin-only: it carries the certificate NAME, and
				// ListDomainsByApplication returns only a bare certificate_id,
				// so this is the one place a member can find out which
				// certificate their own domain is serving.
				r.Get("/api/domains/tls", h.ListDomainTLSStatus)

				// Operator-health stat strip (member-scoped; admins see host + backups)
				r.Get("/api/stats", h.GetStats)

				// Notifications — per-user feed (recipient is the current user).
				r.Get("/api/notifications", h.ListNotifications)
				r.Get("/api/notifications/unread-count", h.UnreadNotificationCount)
				r.Post("/api/notifications/{notificationId}/read", h.MarkNotificationRead)
				r.Post("/api/notifications/read-all", h.MarkAllNotificationsRead)
			})

			// Project/application runtime metrics snapshot: its own scope so a
			// Prometheus-style token narrowed to "metrics" (and nothing else)
			// can still reach it — "read" or "write" also satisfy it (see
			// middleware.scopeGrants), so this changes nothing for a general
			// token, only adds a narrower option.
			r.With(middleware.RequireScope("metrics"), middleware.RequirePinnedProjectMatch()).
				Get("/api/projects/{projectId}/metrics", h.GetProjectMetrics)

			// Deploy actions: a runtime operation on an already-stored
			// application or database, distinct from writing its
			// configuration. Narrower than "write" on purpose — the design's
			// own CI use case ("let CI deploy app X") should not also hand out
			// the ability to rewrite env vars or delete a backup. "write"
			// still satisfies this (see middleware.scopeGrants), so nothing
			// with a general-purpose token changes.
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireScope("deploy"))
				r.Use(middleware.RequirePinnedProjectMatch())

				r.Post("/api/projects/{projectId}/applications/{applicationId}/deploy", h.DeployApplication)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/stop", h.StopApplication)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/start", h.StartApplication)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/restart", h.RestartApplication)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/reload", h.ReloadApplication)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/rebuild", h.RebuildApplication)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/build", h.BuildApplication)
				r.Post("/api/projects/{projectId}/applications/{applicationId}/rollback", h.RollbackDeployment)

				r.Post("/api/projects/{projectId}/databases/{databaseId}/stop", h.StopDatabase)
				r.Post("/api/projects/{projectId}/databases/{databaseId}/start", h.StartDatabase)
				r.Post("/api/projects/{projectId}/databases/{databaseId}/restart", h.RestartDatabase)
				r.Post("/api/projects/{projectId}/databases/{databaseId}/reload", h.ReloadDatabase)
			})

			// Read-only MCP server (0.1.x #4): a stateless JSON-RPC POST, not
			// a REST resource, so RequireScopeByMethod would demand "write"
			// for it — the exact inverse of a read-only server. Given its
			// own explicit "read" scope instead, the same carve-out shape as
			// the metrics/deploy groups above. RequireToken — the mirror
			// image of RequireSession — additionally shuts out a session JWT
			// entirely: MCP clients are machines, and CSRF already exempts
			// the Bearer branch, so a JWT reaching this route would skip
			// CSRF with no compensating control.
			r.With(middleware.RequireToken(), middleware.RequireScope("read")).Post("/mcp", h.HandleMCP)

			// Admin-only: metrics snapshots, settings, cleanup, audit
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireRole("admin"))

				// Split the same way as the non-admin metrics route above:
				// GetMetrics/GetHostHistoricalMetrics accept a "metrics" token,
				// everything else in this block defaults to read/write by method.
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireScope("metrics"))
					// Resource counts, not metrics — hence /api/summary rather than
					// /api/metrics, which left it one character from the Prometheus
					// scrape at /metrics and adjacent to the real host time-series
					// below. The scope stays "metrics": it is the most permissive
					// requirement in the lattice, so no existing token loses access.
					r.Get("/api/summary", h.GetSummary)
					r.Get("/api/metrics/host", h.GetHostHistoricalMetrics)
					// Prometheus scrape endpoint. When METRICS_BIND is configured
					// the metrics are also exposed anonymously on that listener;
					// this admin-gated copy is for operators browsing via the UI.
					// Scoped to "metrics", not RequireScopeByMethod's "read" — a
					// metrics-only token is explicitly sold as a scraper token
					// (see api-tokens-card.tsx) and must be able to reach the one
					// route that description promises. h.ServeMetrics, not
					// metrics.Handler() registered directly — a bare third-party
					// http.Handler has no name reflection can recover (this
					// generated the meaningless operationId "func1") and no doc
					// comment a //apidoc:tag directive could attach to.
					r.Get("/metrics", h.ServeMetrics)
				})

				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireScopeByMethod())

					r.Post("/api/maintenance/cleanup", h.TriggerCleanup)
					// Platform configuration, session-only. UpdateSettings is the one
					// that actually matters: it writes ANY key by name (a handful are
					// validated in a switch, everything else passes straight to
					// UpsertSetting with only an empty-key check) — and
					// host_shell_enabled, the flag that turns on the in-UI host shell,
					// is itself a setting (see hostshell.go's settingHostShellEnabled).
					// Without this gate, a write-scoped admin PAT could flip the most
					// security-critical flag in the product with no human at a
					// keyboard; opening a session from there still needs the password
					// and a second factor, but a token should never reach the switch at
					// all. The same endpoint also sets the dashboard's own domain and
					// TLS mode — where Caddy gets its certificate from. ListSettings and
					// the SMTP endpoints are gated alongside it for consistency, not
					// because they leak a credential: GetSMTPSettings already masks the
					// password to a presence flag, and ListSettings already skips it —
					// but ListSettings still dumps every other key (host shell flag,
					// dashboard domain/TLS, public IP, backup schedule), which is
					// config disclosure and posture recon a leaked token shouldn't get.
					r.With(middleware.RequireSession()).Get("/api/settings", h.ListSettings)
					r.With(middleware.RequireSession()).Put("/api/settings", h.UpdateSettings)
					// SMTP config: dedicated endpoints so the password stays
					// keyring-encrypted and masked (never in the generic settings list).
					r.With(middleware.RequireSession()).Get("/api/settings/smtp", h.GetSMTPSettings)
					r.With(middleware.RequireSession()).Put("/api/settings/smtp", h.UpdateSMTPSettings)
					r.With(middleware.RequireSession()).Post("/api/settings/smtp/test", h.TestSMTPSettings)
					r.Get("/api/requests", h.ListAllRequestLogs)
					r.Get("/api/requests/summary", h.GetAllRequestsSummary)
					r.Get("/api/server/services", h.GetServerServices)
					// Live TLS state of the dashboard's own domain.
					r.Get("/api/server/dashboard-tls", h.GetDashboardTLS)
					// Read-only Docker inspect pages (containers/images/volumes/networks).
					r.Get("/api/docker/overview", h.GetDockerOverview)
					r.Get("/api/docker/containers", h.ListDockerContainers)
					r.Get("/api/docker/images", h.ListDockerImages)
					r.Get("/api/docker/volumes", h.ListDockerVolumes)
					r.Get("/api/docker/networks", h.ListDockerNetworks)
					r.Get("/api/audit-logs", h.ListAuditLogs)
					r.Get("/api/audit-logs/actions", h.ListAuditActions)
					r.Get("/api/audit-logs/export", h.ExportAuditLogs)
					// GET the noun for status, POST noun+verb for the action —
					// the same shape as the queue pair below. Not
					// .../proxy/reconciler beside .../proxy/reconcile: two
					// sibling paths one letter apart is the trap /api/metrics
					// and /metrics already were.
					r.Get("/api/maintenance/proxy", h.GetProxyReconcilerStatus)
					r.Post("/api/maintenance/proxy/reconcile", h.ReconcileProxy)
					r.Get("/api/maintenance/queue", h.GetQueueStatus)
					r.Post("/api/maintenance/queue/clear", h.ClearQueue)
					r.Post("/api/maintenance/queue/clear-pending", h.ClearPendingQueue)
					r.Get("/api/maintenance/logs", h.GetPlatformLogs)
					// A public fact (the address the box is reachable at), legitimately
					// useful to a provisioning script — stays PAT-callable, unlike its
					// neighbors below.
					r.Get("/api/maintenance/server-ip", h.GetServerIP)
					// Session-only: restarting a service and opening a host shell are
					// both maintenance actions on the box itself, not application
					// deploys — see the settings block above for the fuller reasoning
					// (this pair sits in the same "platform configuration and control,
					// not app management" category). CreateHostShellSession was already
					// triple-gated (host_shell_enabled, admin role, step-up re-auth) —
					// RequireSession closes the remaining gap: a PAT could still reach
					// its handler and get exactly as far as "password required".
					r.With(middleware.RequireSession()).Post("/api/maintenance/restart", h.RestartService)
					r.With(middleware.RequireSession()).Post("/api/maintenance/host-shell", h.CreateHostShellSession)
					// Same triple-gate shape as host-shell above (a capability check —
					// here, an update being available at all — admin role, step-up
					// re-auth), and the same reason for RequireSession: this is a
					// platform-control action, and a token should never reach even the
					// "password required" response.
					r.With(middleware.RequireSession()).Post("/api/maintenance/update", h.TriggerSelfUpdate)
					// Session-gated like the trigger above rather than left open to a
					// read-scoped token: it reports whether an update is in flight and
					// quotes the helper's own output on failure, which is operational
					// detail about the host, not something a script needs.
					r.With(middleware.RequireSession()).Get("/api/maintenance/update/status", h.GetSelfUpdateStatus)
					// On-demand manifest check. Session-gated like its siblings: it
					// makes this install reach out to belune.dev, which is the
					// operator's call, not a script's.
					r.With(middleware.RequireSession()).Post("/api/maintenance/update/check", h.TriggerUpdateCheck)
					// ⚠️ Reads stay token-reachable, narrowed to a pinned
					// token's pin (the project arrives as {scopeId}, so
					// RequirePinnedProjectMatch cannot do it — the handlers and the
					// list query do). Checking your own limits from a script is
					// a legitimate use.
					//
					// ⛔ WRITES REQUIRE A SESSION. A quota decides whether
					// another project can deploy at all, so capping one is an
					// operator decision in the same family as issuing
					// credentials or opening the host shell — not something a
					// leaked token should be able to do. This TIGHTENS what a
					// PAT may reach, which the versioning contract permits on a
					// patch release and requires be listed under Breaking
					// changes.
					r.Get("/api/quotas", h.ListQuotas)
					r.Get("/api/quotas/{scope}/{scopeId}", h.GetQuota)
					r.With(middleware.RequireSession()).Put("/api/quotas/{scope}/{scopeId}", h.UpsertQuota)
					r.With(middleware.RequireSession()).Delete("/api/quotas/{scope}/{scopeId}", h.DeleteQuota)
					// Centralised TLS certificate store (upload once, use per-domain)
					r.Get("/api/certificates", h.ListCertificates)
					r.Post("/api/certificates", h.UploadCertificate)
					// Deletable with an admin, write-scoped token — not
					// session-only like the project/app/db/volume/domain/backup
					// set below. domains.certificate_id is ON DELETE RESTRICT
					// (migration 000035), so a cert any domain still serves
					// can't be deleted at all; only an unused cert is reachable
					// here, and it is re-uploadable from the same PEM material
					// that created it, unlike a dropped database.
					r.Delete("/api/certificates/{certificateId}", h.DeleteCertificate)
					// Notification channels: route existing events out to providers.
					r.Get("/api/notification-events", h.ListNotificationEvents)
					r.Get("/api/notification-channels", h.ListNotificationChannels)
					r.Post("/api/notification-channels", h.CreateNotificationChannel)
					r.Post("/api/notification-channels/test", h.TestNotificationChannelParams)
					r.Put("/api/notification-channels/{channelId}", h.UpdateNotificationChannel)
					r.Patch("/api/notification-channels/{channelId}", h.SetNotificationChannelEnabled)
					r.Delete("/api/notification-channels/{channelId}", h.DeleteNotificationChannel)
					r.Post("/api/notification-channels/{channelId}/test", h.TestNotificationChannel)
					// Git provider app configs (per-instance GitHub App / OAuth clients)
					r.Get("/api/git/providers", h.ListGitProviderConfigs)
					r.Put("/api/git/providers", h.SaveGitProviderConfig)
					r.Delete("/api/git/providers/{configId}", h.DeleteGitProviderConfig)
					r.Get("/api/git/providers/github/manifest", h.GetGitHubAppManifest)
				})
			})
		})

		// Env routes: 5 MB body limit + 15 s timeout (bulk import may exceed 1 MB).
		r.Group(func(r chi.Router) {
			r.Use(middleware.BodyLimit(envBodyLimit))
			r.Use(withTimeout(handlerTimeout))
			r.Use(middleware.RequireScopeByMethod())
			r.Use(middleware.RequirePinnedProjectMatch())
			r.Get("/api/projects/{projectId}/env", h.ListProjectEnvVars)
			r.Put("/api/projects/{projectId}/env", h.UpdateProjectEnvVars)
			// See the reveal-endpoint comment above webhook/reveal: same reasoning.
			r.With(middleware.RequireSession()).Get("/api/projects/{projectId}/env/{envVarId}/reveal", h.RevealProjectEnvVar)
			r.Get("/api/projects/{projectId}/applications/{applicationId}/env", h.ListEnvVars)
			r.Put("/api/projects/{projectId}/applications/{applicationId}/env", h.UpdateEnvVars)
			r.With(middleware.RequireSession()).Get("/api/projects/{projectId}/applications/{applicationId}/env/{envVarId}/reveal", h.RevealEnvVar)
		})

		// Databases: standard limit + timeout
		r.Group(func(r chi.Router) {
			r.Use(middleware.BodyLimit(defaultBodyLimit))
			r.Use(withTimeout(handlerTimeout))
			r.Use(middleware.RequireScopeByMethod())
			r.Use(middleware.RequirePinnedProjectMatch())
			r.Get("/api/projects/{projectId}/databases", h.ListDatabases)
			r.Post("/api/projects/{projectId}/databases", h.CreateDatabase)
			r.Get("/api/projects/{projectId}/databases/{databaseId}", h.GetDatabase)
			// Live connection credentials — same reasoning as the /reveal
			// endpoints above: a decrypted secret returned in plaintext needs a
			// session, not just read scope.
			r.With(middleware.RequireSession()).Get("/api/projects/{projectId}/databases/{databaseId}/credentials/reveal", h.RevealDatabaseCredentials)
			r.Get("/api/projects/{projectId}/databases/{databaseId}/volume", h.GetDatabaseVolume)
			r.Put("/api/projects/{projectId}/databases/{databaseId}", h.UpdateDatabase)
			r.Post("/api/projects/{projectId}/databases/{databaseId}/external-access", h.SetDatabaseExternalAccess)
			r.Get("/api/projects/{projectId}/databases/{databaseId}/backups", h.ListDatabaseBackups)
			r.Post("/api/projects/{projectId}/databases/{databaseId}/backups", h.BackupDatabase)
			r.With(middleware.RequireSession()).Delete("/api/projects/{projectId}/databases/{databaseId}/backups/{backupId}", h.DeleteDatabaseBackup)
			r.With(middleware.RequireSession()).Post("/api/projects/{projectId}/databases/{databaseId}/restore", h.RestoreDatabase)
			r.Get("/api/projects/{projectId}/databases/{databaseId}/restores", h.ListDatabaseRestores)
			r.Get("/api/projects/{projectId}/databases/{databaseId}/logs/history", h.ListDatabaseLogs)
			r.Get("/api/projects/{projectId}/databases/{databaseId}/logs/sessions", h.ListDatabaseLogSessions)
			r.Post("/api/projects/{projectId}/databases/{databaseId}/upgrade", h.UpgradeDatabase)
			r.Get("/api/projects/{projectId}/databases/{databaseId}/deletion-impact", h.GetDatabaseDeletionImpact)

			// Backups whose database is gone. Project-scoped because the
			// tombstone they hang off is — the project is the access boundary,
			// so an orphaned backup has no owner above it.
			r.Get("/api/projects/{projectId}/orphaned-backups", h.ListProjectOrphanedDatabaseBackups)
			r.With(middleware.RequireSession()).Post("/api/projects/{projectId}/orphaned-backups/{backupId}/restore", h.RestoreDatabaseFromTombstone)
			r.With(middleware.RequireSession()).Delete("/api/projects/{projectId}/orphaned-backups/{backupId}", h.DeleteOrphanedDatabaseBackup)
			r.With(middleware.RequireSession()).Delete("/api/projects/{projectId}/databases/{databaseId}", h.DeleteDatabase)

			// Scheduled backup configurations per database
			r.Get("/api/projects/{projectId}/databases/{databaseId}/backup-configs", h.ListDatabaseBackupConfigs)
			r.Post("/api/projects/{projectId}/databases/{databaseId}/backup-configs", h.CreateDatabaseBackupConfig)
			r.Put("/api/projects/{projectId}/databases/{databaseId}/backup-configs/{configId}", h.UpdateDatabaseBackupConfig)
			r.Delete("/api/projects/{projectId}/databases/{databaseId}/backup-configs/{configId}", h.DeleteDatabaseBackupConfig)
			r.Post("/api/projects/{projectId}/databases/{databaseId}/backup-configs/{configId}/run", h.RunDatabaseBackupConfig)

			// Project-scoped backup destinations (managed by project members)
			r.Get("/api/projects/{projectId}/backup-destinations", h.ListBackupDestinations)
			r.Post("/api/projects/{projectId}/backup-destinations", h.CreateBackupDestination)
			r.Post("/api/projects/{projectId}/backup-destinations/test", h.TestBackupDestinationParams)
			r.Put("/api/projects/{projectId}/backup-destinations/{destId}", h.UpdateBackupDestination)
			r.Delete("/api/projects/{projectId}/backup-destinations/{destId}", h.DeleteBackupDestination)
			r.Post("/api/projects/{projectId}/backup-destinations/{destId}/test", h.TestBackupDestination)

			// Project backup activity (recent runs across the project's databases)
			r.Get("/api/projects/{projectId}/backups", h.ListProjectBackups)
		})

		// Streaming routes: SSE / long-poll — no timeout, no body limit.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireScopeByMethod())
			r.Use(middleware.RequirePinnedProjectMatch())
			r.Get("/api/projects/{projectId}/applications/{applicationId}/deployments/{deploymentId}/build-logs", h.StreamBuildLogs)
			r.Get("/api/projects/{projectId}/applications/{applicationId}/logs", h.StreamLogs)
			r.Get("/api/projects/{projectId}/applications/{applicationId}/requests/stream", h.StreamRequestLogs)
			r.Get("/api/notifications/stream", h.StreamNotifications)
		})
		// Application metrics stream: same "metrics" carve-out as the
		// snapshot endpoint above, kept outside the group above because it
		// needs a different scope than the blanket read/write default.
		r.With(middleware.RequireScope("metrics"), middleware.RequirePinnedProjectMatch()).
			Get("/api/projects/{projectId}/applications/{applicationId}/metrics/stream", h.StreamApplicationMetrics)
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireRole("admin"))
			r.With(middleware.RequireScope("metrics")).Get("/api/metrics/host/stream", h.StreamHostMetrics)
			r.With(middleware.RequireScopeByMethod()).Get("/api/requests/stream", h.StreamAllRequestLogs)
		})
	})
}
