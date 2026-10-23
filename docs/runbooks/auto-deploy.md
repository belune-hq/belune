# Auto-Deploy Runbook

Belune can deploy an application without anyone clicking **Deploy**. There are
three mechanisms, and which one you want depends on where the trigger comes
from — your git host, or your CI.

| Mechanism | Trigger | App types | Configured in |
|---|---|---|---|
| Push webhook | Your repo receives a push | git only | Deployments tab → Auto Deploy |
| Provider webhook | Same, via a connected GitHub/Gitea/GitLab account | git only | Git (sidebar) |
| Deploy hook | Anything that can `POST` a URL | **git and image** | Deployments tab → Auto Deploy |

Image-based applications — which includes every app created from a template —
have no push to react to, so the deploy hook is their only automatic path.

---

## 1. Push webhooks (git apps)

Add `https://<your-belune-host>/api/webhooks/push` as a webhook in your
repository, paste the same secret on both sides, and pushes to the configured
branch deploy automatically.

**The secret proves authenticity, not identity.** This trips people up. Belune
identifies *which* application a delivery belongs to by matching the payload's
repository URL against the app's configured repo. The secret only verifies the
delivery genuinely came from your git host, via HMAC-SHA256. Two apps pointing
at the same repository both deploy from one push — that is by design (and is how
a repo can back several environments).

A delivery with no matching application, or one whose signature does not verify,
is dropped silently with a `200`. Git hosts retry and alert on non-2xx, and a
webhook that fires for an app you deleted is not an error worth paging anyone
about.

### Supported hosts

| Host | Event header | Signature |
|---|---|---|
| GitHub | `X-GitHub-Event` | `X-Hub-Signature-256` (HMAC-SHA256) |
| Gitea | `X-GitHub-Event` (also sends `X-Gitea-Event`) | `X-Hub-Signature-256` |
| GitLab | `X-Gitlab-Event` | `X-Gitlab-Token` (shared token, compared in constant time) |

Gitea works because it deliberately mirrors GitHub's headers and payload shape.

### Branch filtering

An application tracks one branch, set by the **Branch** field on its Settings
tab. That single field decides both what gets built and which pushes deploy, so
the two can never disagree. Pushes to any other branch are ignored — unless they
match a configured preview branch pattern, in which case a preview app is
materialised and deployed instead.

Leave Branch empty to track the repository's **default branch**. That is what
every application created before this field existed does, and it remains the
default for new ones.

> **If your default branch is not `main`.** Applications created before branch
> selection have no branch recorded, so their push filter falls back to `main`
> while the build clones the repository's default ref. On a `master`-default
> repo that combination silently ignores every push. Set Branch explicitly and
> both halves line up.

### Duplicate deliveries

Git hosts retry. A repeat delivery of the same commit within a short window is
recognised and reuses the existing deployment rather than building twice.

---

## 2. Provider webhooks (connected accounts)

If you connect a git account under **Git** in the sidebar, deliveries arrive at
`/api/git/webhooks/{provider}` and are verified against the provider app's own
secret instead of a per-application one.

- **GitHub App** — the webhook is registered for you when the App is installed.
  Nothing to paste.
- **OAuth providers** — Belune does **not** register the webhook for you. You
  must still add the push webhook URL to each repository by hand, as in section
  1. This is a known gap, not an oversight in your setup.

---

## 3. Deploy hooks (any app type)

A deploy hook is a private URL that triggers a deploy when you `POST` to it. It
carries no payload and needs no credentials beyond the URL itself, so any CI job
can call it with one line.

Enable it on the **Deployments** tab with the switch at the top right of the
Auto Deploy card. The URL is issued immediately; it stays hidden on later visits
behind **Show URL**.

```bash
curl -X POST https://<your-belune-host>/api/webhooks/deploy/<token>
```

GitHub Actions, after pushing an image:

```yaml
- name: Trigger Belune deploy
  run: curl -fsS -X POST "${{ secrets.BELUNE_DEPLOY_HOOK }}"
```

Use `-f` so a failed trigger fails the job, and keep the whole URL in a secret —
the token is in the path.

### What a call does

| App type | Effect |
|---|---|
| Image | Re-pulls the configured tag, resolves it to a fresh digest, recreates the container |
| Git | Runs a normal deploy: clone the tracked branch, build, recreate |

The hook deliberately re-pulls rather than reusing the running image's pinned
digest. A hook fires precisely because CI just pushed a *new* image to the same
tag, so redeploying the old digest would defeat the purpose.

There is **no branch logic**. The caller decides when to fire; branch filtering
stays a push-webhook concept, where the branch is data in the payload.

### Responses

| Code | Body | Meaning |
|---|---|---|
| `202` | `{"status":"queued"}` | Deploy enqueued |
| `202` | `{"status":"in_progress"}` | A deploy was already running; treated as satisfied so retrying CI does not hammer the app |
| `404` | `{"error":"not found"}` | Unknown, disabled, or malformed token |
| `429` | — | Rate limited (30 requests/minute per IP, shared with the other webhook routes) |

The `404` is deliberately identical for all three failure cases so the endpoint
cannot be used to probe which tokens exist.

### Security

The token **is** the credential — anyone holding the URL can trigger a deploy.

- 32 bytes of entropy; stored only as a SHA-256 hash, so a database dump yields
  no working tokens.
- Redacted from Belune's request log and from Caddy's access log, both of which
  would otherwise record the full URL. The proxy container's own log no longer
  carries request lines at all, so it holds no second copy. Note that any
  reverse proxy *in front of* Belune, and your own CI logs, still see it.
- **Regenerate** issues a new token and kills the old one immediately — any CI
  still using it starts getting `404`s.
- Turning the switch off deletes the token permanently; re-enabling issues a
  different one.

> **Do not point a git host's webhook at a deploy hook URL.** It appears to
> work — the host POSTs, Belune deploys — but signature verification and branch
> filtering are both skipped, so *every* branch push deploys to production. Use
> `/api/webhooks/push` for git hosts.

---

## 4. What gets recorded

Every trigger creates a deployment you can see under **Deployments**, tagged
with what caused it: `push`, `hook`, `manual`, `reload`, `rebuild`, `rollback`,
or `template`.

Push-triggered deployments also record the commit's SHA, message, and author,
taken from the webhook payload and shown in both the per-app and global
deployment lists. Deploy-hook deployments have no payload to read, so their
commit column stays empty.

---

## 5. Troubleshooting

**A push does nothing.**

1. Check the branch. This is the most common cause. The **Branch** field on the
   Settings tab must match the branch you pushed; if it is empty, only pushes to
   the repository's default branch deploy.
2. Check the repository URL on the app matches the one in the payload. Matching
   normalises case and a trailing `.git`, but nothing else.
3. Check a webhook secret is set. Without one, nothing can verify, and every
   delivery is rejected.
4. Check your git host's delivery log. A `200` from Belune means the request
   arrived and was processed — it does *not* mean a deploy was triggered.

**A push stopped working after changing the source.** Switching an application
to a prebuilt image clears the repository and the push webhook secret, because
both refer to a repository it no longer builds. Switching back to git requires
setting a new secret — see
[Applying configuration changes](applying-changes.md#changing-where-an-application-comes-from).
The deploy hook is unaffected: it triggers a deploy whatever the source is.

**The deploy hook returns 404.** The token was regenerated, or the hook was
disabled. Open the app's Deployments tab, check the switch, and **Show URL** to
copy the current one.

**The deploy hook returns 202 but nothing deploys.** Look for `in_progress` in
the response — a deploy was already running for that app and the trigger was
folded into it.

---

## Roadmap

Polling a registry for new digests (Watchtower-style) and native registry
webhooks are both under consideration, so an image push could deploy without CI
calling anything. Deploy hooks cover the CI case today, which is why they
shipped first.
