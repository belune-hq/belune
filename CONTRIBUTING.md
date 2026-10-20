# Contributing to Belune

Thanks for your interest in Belune. Bug reports, documentation fixes, and app
templates are all genuinely useful contributions — you do not need to write Go
or React to help.

Belune is pre-1.0 and maintained by one person, so please open an issue or a
[discussion](https://github.com/belune-hq/belune/discussions) before starting
substantial work. It saves everyone the disappointment of a large PR that does
not fit the roadmap.

## Developer Certificate of Origin (DCO)

Every commit must be signed off. Belune uses the
[Developer Certificate of Origin](https://developercertificate.org/) — a short
statement that you wrote the contribution, or otherwise have the right to submit
it under the project's Apache-2.0 licence. There is no CLA to sign and no
copyright to assign.

Add the sign-off automatically with `-s`:

```bash
git commit -s -m "fix(deploy): stop the worker retrying a cancelled build"
```

That appends a line to the commit message:

```
Signed-off-by: Your Name <your.email@example.com>
```

Use a real name (pseudonyms are not accepted) and an address you can be reached
at. Forgot the sign-off? Fix the last commit with:

```bash
git commit --amend -s --no-edit
```

For several commits, rebase and sign them all off:

```bash
git rebase --signoff main
```

A CI check enforces this on every pull request.

## Development setup

**Prerequisites**

- Go 1.25+
- Node.js 22+
- Docker and Docker Compose
- [Task](https://taskfile.dev/) — `go install github.com/go-task/task/v3/cmd/task@latest`
- [Air](https://github.com/air-verse/air) — `go install github.com/air-verse/air@latest`
- [golang-migrate](https://github.com/golang-migrate/migrate) CLI
- [sqlc](https://sqlc.dev/) CLI

**Quick start**

```bash
cp .env.example .env    # 1. environment config
task dev:infra          # 2. Postgres, Redis, Caddy, BuildKit
task db:migrate:up      # 3. database migrations
task dev:api            # 4. API with hot reload  → http://localhost:8080
task dev:web            # 5. Vite dev server      → http://localhost:5173
```

**Devcontainer (recommended)**

The VS Code devcontainer runs the Go API inside a container joined to the
`belune-infra` network, which gives it real Docker DNS (Postgres, Redis, and
Caddy reachable by service name) and lets Caddy route `localhost:80` to the API.
Vite stays on the host for fast HMR.

```bash
docker network create belune-infra 2>/dev/null || true
# VS Code: Ctrl+Shift+P → "Dev Containers: Reopen in Container"
task db:migrate:up
task dev:api        # inside the devcontainer
task dev:web        # on the host, separate terminal
```

`task dev:infra` and the devcontainer are **mutually exclusive** — both bind
ports 80, 443, 5432, and 6379. Stop one before starting the other.

## Project layout

- `apps/api/` — Go backend (Chi, sqlc, Asynq)
- `apps/web/` — React frontend (TanStack Router/Query/Form, Tailwind, shadcn/ui)
- `apps/site/` — belune.dev landing page and documentation
- `infra/` — Docker Compose and Caddy configuration
- `docs/` — runbooks and operator documentation

See [`belune-project-structure.md`](belune-project-structure.md) for the full
architecture.

## Code style and checks

```bash
task fmt     # gofmt + Prettier
task lint    # golangci-lint + ESLint
task test    # Go and web test suites
```

- **Go**: `gofmt`, `golangci-lint`. Handlers stay thin where practical; business
  logic belongs in services.
- **TypeScript**: Prettier, ESLint. Note that `npx tsc --noEmit` checks *nothing*
  in this repo (the root tsconfig is solution-style) — use `task build:web`, or
  `tsc -b --noEmit`.
- **Database**: schema changes are forward-only migrations under
  `apps/api/internal/migrations/`, followed by `task generate:sqlc`. Do not edit
  generated files by hand.
- **Commits**: [Conventional Commits](https://www.conventionalcommits.org/) —
  `feat(scope):`, `fix(scope):`, `docs:`, `refactor:`, `chore:`. Release notes
  are generated from these.

## Pull requests

- One logical change per PR; keep unrelated refactors out of it.
- Explain **why**, not just what. Link the issue it addresses.
- Say how you tested it. For anything touching deploys, databases, or backups,
  test against a real dev stack — mocks do not catch Docker behaviour.
- Update the docs when behaviour changes.
- CI must pass, and every commit needs its DCO sign-off.

## Adding an app template

The one-click app catalog is a set of declarative manifests. Adding one is a
good first contribution — no Go or React changes required.

1. **Create the manifest.** Add a JSON file under
   `apps/api/internal/template/catalog/manifests/<id>.json`. The schema lives at
   `apps/api/internal/template/catalog/schema.json` (point your editor at it for
   autocomplete). A template instantiates native Belune objects — prebuilt-image
   applications, managed databases, volumes, env vars, and a domain — so prefer a
   single upstream image (use an all-in-one image where the project offers one).

2. **Use only these placeholders** (the whole templating language):

   | Placeholder | Resolves to |
   |---|---|
   | `{{secret 32}}` | A fresh random secret of N characters, generated at deploy |
   | `{{input.KEY}}` | A value the wizard collects (declare it under `inputs`) |
   | `{{db.NAME.url}}` | Connection URL of the managed database named NAME |
   | `{{db.NAME.host}}` / `.port` / `.user` / `.password` / `.database` | Individual connection fields |
   | `{{domain.url}}` / `{{domain.host}}` | The hostname the user picks in the wizard |

   Referencing `{{domain.*}}` makes the hostname required in the wizard. Managed
   databases support the `postgres`, `mysql`, `redis`, and `mongo` engines
   (the `other` engine is not yet available from templates).

3. **Configure the health check** (optional but recommended). Each service takes
   a `health_check` block that Belune probes after deploy — a pass marks the app
   healthy, a fail rolls the deploy back:

   ```jsonc
   "health_check": {
     "path": "/healthz",       // probed on the service `port`; must start with /
     "timeout_seconds": 300,   // optional; retry window before failing (default 120)
     "expect_status": 200      // optional; require this exact status (default: any 2xx)
   }
   ```

   Omit the whole block to skip health checking. Raise `timeout_seconds` for apps
   that run migrations on first boot (Metabase, for example). Set `expect_status`
   only when a healthy root legitimately returns a non-2xx code.

4. **Validate.** `go test ./internal/template/...` runs the schema and
   referential checks (undeclared placeholder references, `depends_on` cycles,
   duplicate names, etc.) over every manifest in the catalog. CI runs the same
   test, so a malformed manifest fails the build.

5. **Smoke-test it yourself.** Before opening the PR, deploy the template on a
   dev stack end to end: instantiate it, confirm the app reaches its UI and works,
   confirm any managed database provisions, and confirm deleting the project
   cleans everything up. Note the image tag and health-check path you verified in
   the PR description.

Logos are hotlinked from `logo_url` in v1; if the project has no stable logo URL,
omit the field and the UI falls back to a generic icon.

## Security issues

Do not open a public issue. See [SECURITY.md](SECURITY.md) for private
disclosure.

## Licence

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), and you certify their origin under the DCO as
described above.
