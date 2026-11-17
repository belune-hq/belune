# AGENTS.md

Belune — a self-hosted PaaS that ships as **one Go binary with the React SPA embedded**.
Monorepo: `apps/api` (Go 1.26 · chi · pgx · sqlc · asynq), `apps/web` (React 19 · Vite · TanStack Router/Query · Tailwind 4 · shadcn/base-ui), `apps/site` (Next.js + Fumadocs, belune.dev).

## ⛔ Never

Every line here has already cost a release or produced a silently-green board.

- **`npx tsc --noEmit`** — the root tsconfig is solution-style (`files: []`), so it checks **nothing** and exits 0 in 0.6s. Use `tsc -b --noEmit` or `task build:web`.
- **Hand-edit `apps/api/internal/store/generated/**` or `apps/web/src/routeTree.gen.ts`** — regenerate them.
- **Hand-roll JSON** — responses go through `writeJSON(w, status, data)` / `writeError(w, status, msg)` from `handler.go`, nothing else.
- **`crypto.randomUUID()` or `navigator.clipboard`** — **secure-context only**, so both are `undefined` on any install reached as `http://<hostname>` or `http://<ip>`, i.e. every install before TLS. Use `newClientId()` / `copyToClipboard()` from `src/lib/utils/`, and check MDN for a "Secure context" banner before reaching for a new `window`/`navigator`/`crypto` API.
- **Hand-roll a `<label>`** — use the `Label` primitive; a raw one silently loses the disabled-state and `select-none` handling `Label` already carries.
- **`text-white` on the accent** → `text-brand-fg`. **`text-primary` for accent text** → `text-brand-text`. See *Colour*.
- **One `useState` per form field** — `@tanstack/react-form` with field validators. The hand-rolled dialogs are legacy being migrated out; ⚠️ their actual cost is having **no client-side validation at all**.
- **`*asynq.Client` directly** — enqueue through the `TaskEnqueuer` interface; tests mock it.
- **Drop a column or table, or rewrite existing rows, in a migration** — `update.sh` runs migrations against live installs, and the container has already restarted by then, so a `.down.sql` could not save them either. Preserve and backfill in place, or ship a documented upgrade path. Purely additive changes need no ceremony.
- **Call a UI change done on a green Vitest run** — see *Verifying*.
- **Push to `main`, or use `--admin`** — an agent branches and opens a PR for every change, however small, regardless of who directed it.
- **Sign a commit off as yourself** — DCO sign-off carries the identity of the person who directed the work, exactly as `git log` shows it. ⚠️ `dco.yml` checks the *shape* only, so an agent's own name passes green and still has to be rewritten.
- **`git checkout` someone else's branch in the working tree** — assume it is shared with other agents and with the person directing you. Create a worktree under `~/selfhost-paas-wt/<name>` instead.
- **`docker restart`, or `compose up|down|restart`, the dev stack** (`infra-*` containers) — throwaway auxiliary containers are fine.
- **`gh issue create`** — this project does not file public issues.

## Commands

Run `task` from the repo root; `Taskfile.yml` loads `.env`.

| Task | What it does |
|---|---|
| `task dev:infra` | Postgres + Redis + Caddy via compose (start this first) |
| `task dev:api` / `task dev:web` | Air hot-reload API / Vite dev server |
| `task build` | `build:web` (copies to `apps/api/web/dist/`) then `build:api` |
| `task test` / `test:api` / `test:web` | Go `./...` + Vitest |
| `task lint` / `task fmt` | golangci-lint + ESLint / gofmt + Prettier |
| `task db:migrate:create -- <name>` | New migration pair |
| `task db:migrate:up`, `task generate:sqlc` | Apply migrations, regenerate query code |

Single Go test: `cd apps/api && go test -count=1 -timeout=300s ./internal/handler/ -run TestName`.

⚠️ **Handler/worker/store tests are integration tests — Docker must be running.** `internal/{handler,service,worker,logcollector,store}` each start Postgres in `TestMain` unconditionally and `log.Fatal` without Docker, so they fail at setup rather than skipping; `internal/service/backup` starts a LocalStack container per test. ⚠️ **`-short` is inert** — nothing in this module calls `testing.Short()`, though CI passes it. The heavier drills gate on env vars instead: `BELUNE_DOCKER_INTEGRATION=1` (worker S3/restore), `BELUNE_CADDY_INTEGRATION=1` (proxy). ⚠️ **Always `-count=1`** — the cache silently replays passes for tests that read files outside the module.

## Architecture

Request path: `internal/server/routes.go` → `internal/handler/*.go` → `internal/service/*.go` → `internal/store/generated` (sqlc). Async work: `internal/worker/*.go` (asynq, task names in `worker/tasks.go`). Live updates: `internal/ws` hub, broadcast by channel string (`metrics:host`, `container-logs:{id}`).
Frontend mirrors it: `src/lib/api/<resource>.ts` (fetch) → `src/lib/hooks/use-<resource>.ts` (TanStack Query) → `src/routes/_app/**` (file-based routes) → `src/components/<domain>/`.

**Keep handlers thin** — validation, auth check, DTO mapping only. Business logic goes in a service.

## Standards

- Go: `gofmt`; wrap errors with `fmt.Errorf("doing x: %w", err)`; log with `slog`.
- Multi-statement writes go inside `store.WithTx(ctx, pool, func(q *generated.Queries) error {...})`.
- TS: strict mode, `@/*` alias, no `any`, `import type` for types (`verbatimModuleSyntax`).
- Comments explain **why**, not what — match the existing density; this codebase favors a short rationale over a restatement.

## Procedures

**Schema change** → forward-only `apps/api/internal/migrations/0000NN_name.up.sql` (new migrations ship without a `.down.sql`) → edit `internal/store/queries/*.sql` → `task generate:sqlc` → verify with `go run ./cmd/migrate-check` against a fresh DB.

⚠️ **Edited a migration that was already applied? → `task db:verify`.** golang-migrate records the version number alone, so the edited file never re-runs: your database silently lacks what the edit added while `schema_migrations` still reads clean, and `migrate-check` cannot see it (it only proves a *fresh* database applies). `db:verify` diffs the schema the migrations describe against yours — it is how a dev DB that applied `000068` before `picked_up_at` existed was found, after every scheduled-tasks request had been answering 500. ⛔ Safe only **before** release; once a version ships, fix it forward.

**Full-row `UPDATE`** (e.g. `UpdateApplication` sets every source column) → the handler MUST `Get<Resource>` first and fall back to the stored value for each field the request omits. A partial body passed straight through silently clears `source_repo`, `branch`, `root_directory`.

**New endpoint** → register in `internal/server/routes.go` inside the right auth/rate-limit group, wrapped in `withTimeout(handlerTimeout)`; handler + service method; `h.audit(r, action, resourceType, id, details)` for any mutation; integration test in `internal/handler/*_test.go` (start from `resetDB(t)` + `testutil` fixtures).

**New background job** → `Type<Name>` const in `worker/tasks.go`, handler file in `internal/worker/`, register in `worker.go`, enqueue via `TaskEnqueuer`.

**Storing a secret** → encrypt with `cfg.Keyring.Encrypt`, persist to the `*_encrypted` column, mask on read. Add new key columns to `cmd/rewrap` so key rotation covers them. Redact paths/errors with `internal/pkg/redact` before logging.

**Frontend data access** → API fn in `src/lib/api/`, hook in `src/lib/hooks/`, key registered in `src/lib/hooks/query-keys.ts` — invalidate through that object, never an inline array literal.

**New UI component** → reuse `src/components/ui/` (41 shadcn/base-ui primitives, `style: base-nova`, lucide icons); add via the shadcn CLI (`.agents/skills/shadcn` and `.claude/skills/shadcn` wrap it) rather than pasting. `Switch` for toggles, `PageTabs` for tabbed pages.

**Colour** → `text-brand-fg` for text *on* the accent: `--brand-fg` is computed per accent **and** per theme, white in all four combinations today, but that is the current palette, not a promise (`text-white` is only correct on a fixed surface such as the destructive red). `text-brand-text` for the accent *as* text: `--brand` is a **fill** tuned to carry white text and reads at only 3.5–3.6:1 on a dark page, and no single shade satisfies both jobs. Links, `link` buttons, pill/badge text, the active tab icon and hover-to-brand titles take `--brand-text`; fills, borders and soft backgrounds stay on `--brand`. ⬅️ Check every colour decision on **`/dev/theme`** (dev-only) — every primitive, all four accent × mode combinations, side by side.

**Committing** → Conventional Commits with a scope (`feat(api,web):`, `fix(deploy):`) and **DCO sign-off**: `git commit -s`. One logical change per commit.

**Docs** → every `##`/`###`/`####` heading in `apps/site/content/docs/*.mdx` is Title Case.

## Verifying — what each check can and cannot see

⚠️ **Mocks do not catch Docker or Caddy behavior.** Touching deploys, databases, backups or Caddy routing: say so, and smoke-test against a real dev stack before claiming it works.

⚠️ **A green Vitest run is not proof the UI renders.** The `dom` project runs in jsdom: no layout engine, only a partial `getComputedStyle`, so a toast composited at `opacity: 0` satisfies every assertion — `toBeVisible()` included, in Playwright too — while a human sees nothing. **v0.1.16 shipped exactly that.** Real rendering is `task test:browser` (`scripts/smoke-browser.sh`), which boots the production stack and drives it with Playwright from `apps/web/e2e`; it is CI's `Production smoke drill` job, on pull requests as well as pushes.

⚠️ **Playwright matches an accessible name by SUBSTRING.** `exact: true` fixes a name that is a *prefix of other names* and **breaks** one that is a *prefix of the real name* — platform-minted container names carry a random suffix, so `worker-1` is never exact. ⬅️ **Before calling a UI change done, grep `apps/web/e2e` for the accessible names you touched.** Three consecutive smoke-drill failures were names alone, and that habit found five more breaks the first time it ran.

⚠️ **`127.0.0.1` is a secure context**, so the smoke drill cannot see the secure-context class of bug at all — see *Never*.

## Working alongside other agents

⚠️ **Squash-merge means `git log origin/main..branch` is NOT a merge check.** A merged branch still
shows its commits as unique. Compare trees (`git diff origin/main <sha>`) or read the PR state —
and when listing PRs, list **all** states (`--state all`), or a merged one reads as missing.

⚠️ **A stale worktree silently invalidates any measurement.** Compare `HEAD` to `origin/main`
before quoting one.

⚠️ **`protect-main-quality` sets `strict: true` on 4 required checks**, so N open PRs cost N
*serialised* CI cycles. Combine related work by touched file rather than fanning out. Note
`Production smoke drill` is deliberately **not** required, so it can be red on a mergeable PR —
read it anyway.

⚠️ **Integration tests need `DOCKER_HOST` pointed at the OrbStack socket**, not just a running
Docker — `docker info` succeeding proves nothing about what testcontainers will reach.
