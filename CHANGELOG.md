# Changelog

All notable changes to Belune are documented here.

Belune is pre-1.0. The versioning contract while it stays there:

- **Patch releases** (`0.1.0` → `0.1.1`) are always safe to apply **to your
  install**. They never change the deployment topology, never require a host
  action, and never ask anything of you beyond running `update.sh`. Your
  dashboard, your applications, and your data carry over untouched.
- **The API is the one exception, and only until it settles.** A patch release
  may tighten what a personal access token is allowed to reach, or move an
  endpoint that no published reference had promised yet. This reaches scripts
  only — never the dashboard, never your data. Now that the [API
  reference](https://belune.dev/docs/api) is published, a documented path is a
  contract and will not move without a deprecation period; token boundaries may
  still tighten before 1.0. Every such change is listed under **⚠️ Breaking
  changes** at the top of that release's notes, and explained in full here.
- **Minor releases** (`0.1.x` → `0.2.0`) may change the deployment topology
  itself, and may require a host action.
- **Always take a backup before upgrading.** `update.sh` does this for you;
  migrations are forward-only and cannot be undone by downgrading the image.

Release notes for each version are also published on the
[Releases page](https://github.com/weiliang79/belune/releases).

## [0.1.19]

**A security release.** It closes a set of related flaws in how personal access
tokens that are *pinned to specific projects* were enforced. If you have never
created a project-pinned token, nothing here affected you. There is no
migration and no infra restart: run `update.sh` and you are done.

### ⚠️ Breaking changes

**A personal access token can no longer change a quota.** `PUT` and `DELETE` on
`/api/quotas/{scope}/{scopeId}` now require a signed-in session. Reading quotas
from a script still works, and is narrowed to the projects a pinned token is
allowed to see.

A quota decides whether a project can deploy at all, so changing one belongs
with issuing credentials and opening a host shell rather than with routine
automation. If you have a script that sets quotas with a token, it will start
receiving `403` and needs to be run by a person instead. This is the kind of
change the versioning contract at the top of this file permits on a patch
release, and this is it being listed.

### A project-pinned token could reach projects it was not pinned to

**A pin is meant to be a ceiling: a token pinned to one project should never
reach another.** It was not enforced everywhere, and a token with the Admin
role could read — and in two places change — data belonging to projects outside
its pin.

What was reachable: another project's HTTP request logs, its applications'
environment variables and deployment history, its build logs (which commonly
contain build arguments and repository URLs), the install's audit log, and
another project's quotas. Two of the gaps allowed writes rather than reads —
changing another project's quota, and removing a route protection such as an IP
allowlist or basic-auth from another project's live domain.

None of this was reachable without a valid token, and none of it crossed the
boundary into another *install*. The dashboard was never affected: a signed-in
session is not pinned, and an operator signing in has always been able to see
their whole install by design.

**What to do.** Update, and nothing else is required — the tokens themselves
were never exposed. If you issued project-pinned tokens to someone you would
not want reading your other projects, treat what they could reach as having
been readable and rotate them from Account → API Tokens.

**How it is fixed.** Every request that names a resource now checks which
project that resource actually belongs to, rather than trusting the project
named in the address. Where a surface genuinely has no project to check
against — the install-wide audit log — a pinned token is refused it outright
rather than shown all of it.

## [0.1.18]

Two log viewer fixes, both found by using it on a real install. Nothing changes
your install's topology or asks anything of your host: run `update.sh` and you
are done. There is no migration and no infra restart.

### Logs no longer go missing after an application restarts

**Restart an application, watch the logs run from shutting down to starting up,
leave the Logs tab and come back — and everything after the restart was gone.**
The pane ended at the old container's last line, and only a full page refresh
brought the rest back.

Two things combined. The live stream is held by the page, so leaving the tab
discarded it; and the stored history was being served from a cache up to a
minute old, which meant the copy taken *before* the restart. Neither source then
had the new container's output. A refresh worked because it skips the cache
entirely, which is exactly what made this look like a display problem rather
than a fetching one.

Log history is no longer cached. The pane keeps showing what it already has
while it re-reads, so returning to it costs a request rather than a blank
screen.

### Log line colours no longer stop halfway along a long line

**Scroll a long log line sideways and its colouring disappeared** — the red of
an error row, the yellow of a warning, the highlight under the pointer — leaving
the text on bare background from roughly the width of the pane onwards.

Each line was only ever as wide as the visible pane, while its text was free to
run past that. The colour had nothing to sit on beyond the edge. Lines are now
as wide as the longest one in view, so the colouring reaches the end of the text
wherever you scroll to. Wrapping is unaffected.

## [0.1.17]

Nothing in this release changes your install's topology or asks anything of your
host: run `update.sh` and you are done. There is no migration and no infra
restart.

### Container logs no longer show the same line twice

**Leave an application's log viewer open, switch to another tab, come back, and
lines you had already read appeared again** — sometimes out of order, mixed into
the newest output. The viewer reads from two places at once: the stored history
and the live stream. Returning to the tab refetched the history, and because a
line is written to the database *before* it is sent to the viewer, every line
still on screen came back a second time. It was never a race — a refetch was
guaranteed to duplicate.

The two sources are now merged rather than concatenated, matching a line by its
content and the millisecond it was recorded, which is the only thing the two
representations share. A repeated line that the container genuinely printed more
than once is kept, because for a repeated error the count is the message.

Present since the log viewer was built, not introduced by a recent release.

### The dashboard no longer runs out of its own request allowance

**Working normally in two or three tabs could make the dashboard start failing
with "too many requests".** The API limits how much one caller may ask for per
minute, and a signed-in operator counted as one caller no matter how many tabs
they had open — sharing a budget sized for an automated script. A single tab
costs between 7 and 33 requests a minute depending on the page, plus a burst on
every navigation, so three tabs on a busy page reached the limit before anyone
touched anything.

An interactive session now has its own allowance, separate from the one personal
access tokens get, and sized against what the dashboard actually spends. Token
limits are unchanged.

If you do hit a limit, the message now explains itself instead of arriving as a
bare "Too Many Requests", and the dashboard no longer immediately retries a
request that was refused for being over the limit — which previously made the
wait longer.

### The dashboard asks your server for less

Three things it polled for turned out not to be worth their cost, measured
against a real install:

- the per-service runtime snapshot was refetched every five seconds on every
  application tab, including ones that show no live figures, to render a single
  "Up 3d 4h" label that changes once a minute at most;
- the dashboard's own certificate status was re-probed every fifteen seconds for
  as long as the page stayed open, including long after the certificate was
  issued and could not change;
- a failed request was retried even when the answer could not change.

None of this was visible to you. It is less work for the machine Belune runs on,
which on a small server is the point.

### Release notes now describe the whole release

**A release that had release candidates got notes covering only the commits since
the last candidate**, rather than since the last real release — v0.1.16's own
notes listed 2 of its 10 changes. The same truncated range decided two flags that
`update.sh` reads to tell you whether a release contains breaking changes or needs
a host action, so those could have been wrong; they never were, by luck.

## [0.1.16]

### Updating an application through the API no longer clears settings you left out

**If you edit an application with a script or an API client, this release stops it
from silently wiping configuration.** `PUT /api/projects/{id}/applications/{id}`
wrote every source column on every call, so a request that sent only the field it
wanted to change cleared the rest — repository, branch, root directory, Dockerfile
path, builder image. The application kept running, and the loss only surfaced at
the next deployment, when it built from the wrong place or stopped building at all.

It now reads the stored application first and keeps each value you did not send.
Sending a field as an empty string still clears it, deliberately — the difference
between "leave this alone" and "make this empty" is now real instead of accidental.

The dashboard's application settings form now sends the whole source configuration
it renders, blank fields included, so clearing a Dockerfile path or a root directory
there does exactly what it says.

### A failed update tells you so, without reloading the page

**When an update could not start, the Server page went quiet.** It reported that
the update had begun and then showed nothing further — the reason was recorded and
displayed correctly, but only to someone who happened to reload the page. An
operator watching the screen saw a successful start and no outcome.

The failure now announces itself twice over: a notice appears with the reason, and
the Server page keeps a record of it until the next attempt. The elapsed-time notice
that runs during an update clears itself first, so the two never overlap — and the
reason is fetched the moment the update stops rather than on the next poll, so there
is no quiet gap between the two.

### The update progress clock is visible to the person who started the update

**Every other open tab showed a running "Belune is updating" notice; the one that
clicked the button did not.** A confirmation message raised at the same moment sat
on top of it and hid it from view for ten seconds. On a quick failure that was the
entire update, so the only thing the operator ever saw was a message predicting a
restart that never happened.

The confirmation is gone and the progress notice shows immediately instead. It
reports elapsed time and nothing else — never an estimate of how long is left.

### Environment variable changes are recorded in the audit log

**Replacing an application's or project's environment variables produced no audit
entry at all.** It now records one, listing the **names** of the variables added,
changed and removed. Values are never written to the audit log, and neither are
the contents of a secret.

Removals are listed too, because a variable disappearing is the change most likely
to break a deployment and the one hardest to spot afterwards.

### Installs outside /opt/belune no longer get a bogus warning after every update

**If you installed Belune somewhere other than `/opt/belune`, every update ended by
telling you the systemd units had changed when they had not** — and the commands it
offered would have broken your install. `install.sh` rewrites the install path into
those unit files, so the updater was comparing a rewritten copy against an
unrewritten one and could never see a match. Following its advice would have pointed
`WorkingDirectory` and `ExecStart` at `/opt/belune`, a directory that does not exist
on such a host.

The comparison now accounts for the rewrite, so the warning appears only when the
units really did change — and when they have, the commands it prints are the ones
that are correct for your install path. Installs at the default location were never
affected.

### A failed update no longer asks whether the version exists

When a pull failed, the updater asked "Does that version exist?". By that point the
version demonstrably does exist — the updater shipped inside that very image and had
to be fetched to run at all — so the question sent people to check the one thing that
could not be wrong. The message now reports the failure and leaves Docker's own
explanation, printed directly above it, to say why.

### The dashboard makes fewer requests

Saving an application or acting on a database refreshed the same set of data twice
over, roughly doubling the requests each action cost. Noticeable mainly as the
dashboard occasionally reporting "Too many requests" during a run of rapid changes.

### Corrections to the published API reference

The check meant to keep [the API reference](https://belune.dev/docs/api) in step
with the code had not been running since 0.1.7, so the published reference had
drifted. Two corrections land with this release:

- `GET /api/version` now documents its `updating` and `updating_for` fields, which
  have been returned since 0.1.14 but were missing from the reference entirely.
- Seven optional fields on the update-application request are now correctly
  documented as nullable: `source_repo`, `source_image`, `branch`,
  `root_directory`, `dockerfile_path`, `build_type_override` and `builder_image`.

No endpoint moved and no behaviour changed — the reference now matches what the
API has been doing.

### Upgrading

Apply this the usual way. Nothing about your projects, applications, databases,
tokens or backups changes, and no host action is needed.

## [0.1.15]

### The updater is now a program, not a shell script

**Nothing changes about how you update.** The thing that performs an update — resolving
the target, taking the pre-update backup, moving the pinned image, swapping the
version-matched infra files and restarting the stack — is now a compiled program shipped
in the same image, instead of a shell script. The steps, their order, and what aborts the
update are deliberately identical.

**Why it was worth doing.** Three separate bugs in one week came from shell idioms rather
than from logic: a confirmation prompt with nowhere to read an answer from when the update
runs unattended, a command substitution that aborted the script without saying so, and a
script corrupting itself by being overwritten while it was still running. The last of those
was addressed in 0.1.13 by moving the updater into the image; this release takes the shell
out of that path altogether.

**The one thing you might notice** is the wording when an update fails. Messages about files
and directories now read like the standard command-line tools they replaced, so a failure
shown on the Server page may be phrased differently from before. What it tells you, and the
point at which it stops, are unchanged — including that a failure before the restart puts
your pinned version and infra files back exactly as they were.

`scripts/update.sh` is unchanged and still the way to update from a shell. It continues to
hand the work to the target version's updater, so updating from the host and from the
dashboard run the same code and are reported the same way.

### Upgrading

Apply this the usual way. Nothing about your projects, applications, databases, tokens or
settings changes, and no host action is needed.

## [0.1.14]

### Every page now shows you when the platform is updating

Applying an update used to leave everyone else in the dark. Their pages kept
working, then stopped answering, then came back on the new version — with
nothing in between to say why.

Now every open page follows along, whoever is looking at it:

- **While the update prepares** — pulling the new version and taking its
  pre-update backup, which is most of the wall clock — a small notice counts up
  the time elapsed. The dashboard still works throughout, so nothing is blocked.
- **When the platform restarts**, the notice says so and the page waits.
- **When it comes back**, a short countdown runs and the page reloads itself.

The time shown is always **elapsed, never a prediction**. Nothing can know how
long a backup of your database will take, so nothing pretends to.

This reaches **every** signed-in person, not just the administrator who pressed
the button, and it covers an update run with `scripts/update.sh` over SSH just
as well as one started from the dashboard.

### Backups taken by Belune itself could stop working after an update

**On an install that had applied an update, the dashboard's "Back up now" and —
more importantly — your scheduled daily control-plane backup could fail every
time with "a control-plane backup is already in progress", while no backup was
running at all.** Nothing cleared it, so it stayed that way until someone
intervened on the host. If you rely on the in-app schedule for your control
plane, check the Backups panel after upgrading and take one by hand if the
recent runs are failures.

The cause: backups taken from the host — including the one every update takes
before it starts — run as `root` and left the lock file they share with the
dashboard readable but not writable by anyone else. The dashboard runs as an
unprivileged user, so it could never open that file again, and it reported the
failure as a conflicting run rather than as the permission problem it was.

Updating to this release repairs the lock file on your install, and backups now
create it so that both users can open it.

Backups taken on the host were never affected — `scripts/backup.sh` and the
pre-update backup each run as `root`, which can open the file either way. So an
install whose backups all came from the host, or whose schedule happened to run
before its first update, never saw this. Nothing was lost where it did happen:
the archives that did get written are intact, and the failures were recorded as
failed runs rather than passing silently.

### A failed backup now tells you what went wrong

When a remote upload fails, the Backups panel shows **the actual reason** — a
refused connection, a rejected credential, a missing bucket — instead of
"remote upload failed (see output above)", which referred to output that only
ever existed in a terminal nobody was looking at. It also says plainly that the
archive was not copied offsite, and where to check the settings.

Backups run from the host or by an update now record their log too, so the panel
can explain them at all; previously only in-app backups did. And a run's notes
carry the right severity: a backup that deliberately kept going without its
offsite copy reads as a **warning**, not an error, and no longer hides under the
panel's error filter as information.

### Upgrading

Apply this the usual way. Nothing about your projects, applications, databases,
tokens or settings changes, and no host action is needed.

## [0.1.13]

### Updater fixes now reach the update that installs them

Until this release, the code that performed an update was always the copy
already sitting on your host — the version you were updating *from*. A fix to
the updater therefore protected the release *after* the one that contained it,
never the one you were applying.

That is why the last three releases each had to carry a warning about updating
*to* them. The 0.1.11 notes below are the clearest example: the fix for a
broken update shipped **in** 0.1.11, so the update into 0.1.11 still ran the
broken 0.1.10 script and had to be done by hand on the host.

**From here, the updater that runs is the one shipped with the version you are
installing.** A problem found in the update path can be fixed in the next
release and that fix applies immediately, to the very update that delivers it.

### What changed

The updater now lives inside the Belune image rather than only as a script in
your install directory. Applying an update pulls the target version's image
first, then runs the updater out of it.

`scripts/update.sh` is still there and still the way to update from a shell —
`bash scripts/update.sh` behaves exactly as before, including taking a backup
first and printing how to roll back. It is now a short launcher that hands the
work to the target version's updater, so updating from the host and updating
from the dashboard run the same code and are reported the same way. Anything
you have scripted around it keeps working.

Nothing about your projects, applications, databases, tokens or settings
changes, and the update still backs up before it touches anything.

### If an update cannot start

When the target version's image cannot be pulled, the Server page now tells you
the actual reason — a registry rate limit, a name that does not resolve, no
disk space — instead of assuming the version does not exist. Nothing on your
host has been touched when this happens, and you can retry once the cause is
cleared.

### Upgrading

Apply this the usual way. No host action is needed and nothing you were doing
stops working.

One honest note on timing: **this update is still carried out by 0.1.12's
updater**, because that is the code on your host when you start it. The change
above takes effect from your next update onward.

## [0.1.12]

### A Member could read data the dashboard refused them

**On installs with more than one user account, a Member could read live data
from projects they were never given access to** — the host's own CPU, memory
and disk figures, the HTTP request log of every application on the box, and the
container logs of any application or database whose id they knew.

The dashboard's live-data connection handed out whatever channel a client asked
for, without checking whether that client was allowed to hear it. Every other
route to the same data was correct: the REST API refused all three with `403`,
and so did MCP. Only the live connection never asked.

**Single-user installs were never exposed to anyone else** — there was no
second account to do the reading. If you are the only user of your install,
this changed nothing about who could see your data.

Reaching it took deliberate effort rather than a stray click: a client had to
connect and ask for a channel by name, and for another project's logs it had to
already know that application's id. Typing an admin page's URL was not enough
on its own — those pages load data the API refuses a Member, so they collapse
into an error almost immediately.

⚠️ **Container logs are the part worth thinking about.** Application logs
routinely contain connection strings and API keys printed at startup, so a
Member who read another project's logs may have seen its secrets. If you run a
multi-user install and there is anyone on it you would not hand those secrets
to, treat what your applications print at startup as worth rotating.

**Neither we nor your audit log can tell you whether it happened.**
Subscriptions to the live connection were never recorded anywhere, so there is
no history to go back through. Found during internal review rather than from a
report.

### What changed

Every subscription is now authorized before it is registered, against the same
ownership and sharing rules the REST API applies — the same code, not a second
copy of it that could drift. Anything the server does not positively recognise
is refused, so a channel added in a future release stays closed until someone
decides who may hear it.

A token limited to specific projects is now honoured here too. Previously,
pinning a token narrowed what it could reach through the API while leaving the
live connection open to everything — so the pin was not the boundary it
appeared to be.

The admin-only pages — Requests, Server, Docker, Notifications, Team, Quotas
and Audit Log — now have a real access check. The sidebar had always hidden
their links from a Member, but a hidden link is not a guard.

**Upgrading from 0.1.11: breaking only if you drive the live connection with a
token.** A script that opens `/api/ws` keeps working unchanged when it uses an
unpinned admin token. It will now be refused when it uses a Member's token to
read `metrics:host` or `requests:all`, when it uses a project-pinned token to
read anything outside that pin, or when it uses any pinned token to read
`requests:all` — which is a single stream spanning every project and cannot be
narrowed to a pin. A refused subscription is answered on the channel it asked
for, rather than going quiet. The
[WebSockets guide](https://belune.dev/docs/api/overview/websockets) now lists
who may subscribe to each channel.

### Upgrading

Apply this the usual way; nothing about your projects, tokens or settings
changes, and no host action is needed. If you use only the dashboard, the API
or MCP, there is nothing to do and nothing you were doing stops working.

## [0.1.11]

### ⚠️ Updating from 0.1.10? Do this on the host, not from the dashboard

**The update from 0.1.10 will fail partway through.** It is a known, one-time
problem and nothing is lost when it happens — but it is easier to avoid than
to recover from.

**To avoid it**, SSH to the host and run the update from a copy of the script:

```bash
cp /opt/belune/scripts/update.sh /tmp/belune-update.sh
bash /tmp/belune-update.sh v0.1.11
```

**If you already tried and it failed**, you will have seen it stop with a
`command not found` error shortly after `Pinned to 0.1.11`. Your install is
pinned to the new version while still running the old containers. Finish it
with:

```bash
cd /opt/belune && docker compose up -d
```

Your data is untouched: the pre-update backup and the previous infra files
were both written before the failure.

**Why:** `update.sh` replaces itself as part of an update, and the shell reads
a script as it goes — so when the file changes underneath it, it carries on
from the wrong place. This release fixes that, but the fix can only help
updates that _start_ from 0.1.11, because the script running your upgrade is
the one you already have. **This is the last release where it happens.**

### An update no longer stops because a backup could not be uploaded

**If remote backup storage was configured but unreachable, updating from the
dashboard would stop and tell you nothing useful.** The update takes a backup
first, that backup tried to upload offsite, the upload failed, and the update
gave up — with a message asking a question nobody could answer, because there
is no terminal behind the dashboard's update button.

Two things changed. The pre-update backup is a **local** rollback point, so a
failed upload is now a warning rather than a failure: the archive is written,
the update continues, and the backup is recorded as local-only so it cannot be
mistaken for an offsite copy. A backup that fails for a real reason — no disk,
no database — still stops the update, but now says what went wrong and what to
do about it.

Manual and scheduled host backups are unchanged: if you asked for offsite and
did not get it, that is still an error.

### An interrupted update puts itself back

**Two separate ways an update could leave an install worse than it found it,
both fixed.**

`update.sh` replaced itself partway through its own run. Because the shell
reads a script as it goes, it carried on from the same position in the _new_
file and started executing whatever happened to be there — so an update could
fail with a nonsense error after it had already changed things. It had been
harmless only because the file had not changed since 0.1.8; the moment it did,
it broke.

Separately, a failure between pinning the new version and restarting left the
install pointing at the new version while still running the old one — so a
later, unrelated restart would have jumped it forward without warning. The pin
and the infra files are now put back automatically if the update fails before
the restart, which is what the "nothing has changed" message always promised.

A failure _after_ the restart still leaves the recovery instructions rather
than undoing itself: by then migrations may have run, and putting an older
version back on a newer schema would be worse than stopping.

### Remote storage can be tested before you save it

**"Test connection" used to refuse to run until you had already saved and
enabled the settings** — so the only way to find out whether your credentials
worked was to commit them first. It now tests what is in the form.

The old behaviour was also the reason some installs hit the update problem
above: the form pushed you to enable remote storage before you could check it.

### An AI assistant can read build logs

The [MCP server](https://belune.dev/docs/api/overview/mcp) (new in 0.1.10)
gains **`get_deployment_logs`**, so an assistant asked why a deployment failed
can read the build output instead of seeing only "failed". It returns the last
200 lines by default, capped at 1000.

Log output is also cleaner: terminal colour codes are stripped before they
reach the client, and build logs arrive as `<timestamp> <level> <message>`
rather than raw JSON. Both log tools still return the text itself verbatim —
**container and build logs routinely contain connection strings and API keys**,
and nothing here redacts them.

### Smaller things

- A deployment that was still building reported a finish time equal to its
  start, so anything reading it mid-build — the dashboard, the API, an AI
  assistant — saw a build that had apparently finished instantly. The finish
  time is now recorded only when a deployment actually finishes.

### Upgrading

No changes to how Belune is deployed, no host action, and no migrations. Apply
it from **Server → Configuration → Updates**, or run `update.sh` as before.

**⚠️ If you have remote backup storage enabled, read this before updating.**
The fix at the top of these notes protects updates that _start_ from 0.1.11 —
it cannot help the update that installs it, because the copy of `update.sh`
running is the one you already have. So updating **to** 0.1.11 can still stop
with `belune-backup-upload not found` or `Backup failed`. If that happens:

1. **Server → Backups** → turn **off** remote storage
2. Apply the update
3. Turn remote storage back **on**

Nothing is changed or lost when it stops. Running `scripts/update.sh` on the
host also works — there it can ask whether to continue, which it cannot do from
the dashboard.

The same applies to the self-repair fixes above: they protect updates that
_start_ from 0.1.11, so the update **to** 0.1.11 still runs the copy of
`update.sh` you already have.

**If you tried 0.1.11-rc1**, you are running that release candidate's updater,
which still had the self-replacement bug — and your `.env` may name a newer
version than the containers actually running. Check with
`grep BELUNE_IMAGE /opt/belune/.env` against what the dashboard reports, and
run `docker compose up -d` in the install directory if they disagree.

## [0.1.10]

### Connect an AI assistant to your install

Belune now speaks the [Model Context Protocol](https://modelcontextprotocol.io).
An MCP-compatible client — Claude, and others as they add support — can ask
about your projects, applications, databases, deployments, TLS state and
backups, and read a running container's logs.

Set it up from **Account → Connect an AI Assistant**. It mints a token scoped
to Read and hands you the command to paste into your client; there is nothing
to install on either side.

**It is read-only, and not merely by permission.** There is no deploy,
restart, delete or restore tool for a client to call — the tools do not exist,
so a token with full write scope gains nothing by going through MCP. Anything
that changes your install still goes through the dashboard or the API.

⚠️ **Tools that read container logs send those logs to whichever client you
connect, exactly as written.** Logs routinely contain connection strings and
API keys printed at startup. The dialog says so before it issues the token.
Only connect a client you would trust with that.

### Tokens can be limited to specific projects

A personal access token used to reach every project its owner can. When you
create one — under **Account → Personal Access Tokens**, or through the AI
assistant dialog — you can now pin it to one or more projects instead. Every
other project returns `403`, including ones you could otherwise reach yourself.

Pinning only ever narrows a token; it can never grant access its owner does
not already have, and the picker only offers projects you can reach. The pin
is fixed when the token is created, so re-scoping means creating a new token
and deleting the old one — the same pattern as changing a token's scope.

Deleting a project removes it from any token pinned to it. A token pinned to
nothing but deleted projects reaches nothing, rather than falling back to
reaching everything.

### Host backups were never reaching remote storage

**If you turned on remote storage for host backups, it has never worked — on
any version.** The helper that performs the upload was not included in the
Belune image, so `install.sh` reported success while quietly leaving it out,
and the upload was skipped every time. It is in the image now.

This affects the **host/CLI backup path only**: manual runs
(`systemctl start belune-backup.service`) and the safety backup `update.sh`
takes before moving versions. Those archives were still written locally, so
nothing was lost — they just never left the machine.

**Your daily automatic backups were not affected.** Those run in-app and
upload through a different path that has always worked, as do application and
database backups.

⚠️ **Check this after updating.** If you rely on host backups going offsite,
open **Server → Backups**, confirm remote storage is configured, and run a
backup to see the archive arrive at your destination. Until now that
confirmation would have been the first honest signal you had.

### A failed request no longer looks like missing data

**If the API was briefly unreachable, the dashboard would tell you the thing
you were looking at did not exist** — a project page reading "Project not
found", the project list appearing empty. Nothing was ever wrong with your
data; the page simply could not tell a failed request apart from an empty
answer.

This mattered most during an update, when the API restarts and you are most
likely to be watching. Every page now distinguishes the two: a failure says so
and offers to retry, and only a genuine 404 says the thing is gone.

### Smaller things

- The generated [API reference](https://belune.dev/docs/api) documents project
  pinning, and token responses carry the projects a token is pinned to.
- Creating a token validates as you type, like the rest of the dashboard's
  forms.

### Upgrading

No changes to how Belune is deployed and no host action. Apply it from
**Server → Configuration → Updates**, or run `update.sh` as before.

This release adds a database migration. It only adds a table and a column —
nothing existing is changed or removed — and `update.sh` takes its usual
backup first.

**Read the host-backup note above before you update**, since the safety backup
`update.sh` takes will itself be the first one that can reach remote storage.

## [0.1.9]

### Forms tell you what is wrong before you submit

Every form in the dashboard — twenty-one of them, from creating an application
to configuring SMTP — now validates as you type. A required field you left
blank, a port out of range, a mount path that is not absolute, a repository URL
that is neither `https://` nor `git@`: the field turns red, the message sits
under it, and the submit button does nothing until it is fixed. Before this,
most forms sent whatever you typed and let the API's error come back as a
toast.

The two-factor, host-shell and update step-up dialogs validate the same way, so
a mistyped code is caught on the spot instead of after a round trip.

### The dashboard reloads itself after an update

**Updating from the dashboard (new in 0.1.8) left the open tab running the old
version until you refreshed.** It now notices the new version within thirty
seconds — immediately on pages with a live connection — tells you, and reloads.
Every open tab reloads on its own. A manual `update.sh` with the dashboard open
is covered too.

### The accent colour is easier to read

**Emerald is one shade in both light and dark mode now** — a deeper green than
before, chosen so white text on it is legible everywhere (it was not in light
mode). Links, status text and the running-service dot use a separate shade of
whichever accent you picked, tuned to read against the page rather than to
carry text; in dark mode that is noticeably brighter than the buttons. Violet
keeps its colours and gets the same treatment for text.

### Smaller things

- Moving between pages shows a loading placeholder instead of the previous page
  lingering until the next one arrives; opening an application or database from
  a project no longer flashes the project header first.
- Closing any dialog fades out what you were looking at instead of an empty
  box, and reopening it always starts fresh.
- The **Configuration** tab on the Server page groups its cards under
  **Platform** (Updates, Instance, Metrics Retention) and **Operations**
  (Email, Maintenance), and the retention settings sit side by side instead
  of stacked.
- The Instance name and Server IP fields no longer appear twice after a cold
  load of the Configuration tab.

### Upgrading

No migrations, no changes to how Belune is deployed, no host action. Apply it
from **Server → Configuration → Updates**, or run `update.sh` as before.

**One thing to know about this particular update:** the tab you trigger it
from will still be on 0.1.8 when the update lands, because 0.1.8 does not yet
have the reload described above. Refresh once. From 0.1.9 on, the dashboard
handles it.

## [0.1.8]

### Belune can now update itself from the dashboard

**Until now, updating meant SSH.** Belune checks belune.dev once a day for a
newer release, tells you when there is one, and applies it from
**Server → Configuration → Updates** — no terminal required.

- **You are told, not left to wonder.** A dot appears beside the version in the
  sidebar, the Updates card names the release and links to its notes, and
  admins get a notification through whichever channels they already use. A
  release marked breaking says so before you click anything.
- **Updating is deliberate, never automatic.** The button asks for your
  password (and your second factor, if enrolled), takes a full backup first,
  then runs the same `scripts/update.sh` the manual path uses — so the two can
  never drift. Belune restarts itself; the dashboard reconnects on the new
  version.
- **Check now** runs the check immediately instead of waiting for the daily
  sweep, and **Skip this version** silences one release without turning
  checks off.
- **A release that changes host-level configuration cannot be applied from
  the dashboard**, and the card says so instead of offering a button that
  would fail — run `update.sh` on the host for those, as before.

The check is a plain `GET` of a public manifest, sends nothing about your
install, and is on by default. Turn it off with the **Check automatically**
switch on the same card, or set `update_check_enabled` to `false`.

**If an in-app update fails, nothing is lost.** The helper that applies it
keeps its own log, the Updates card reports what went wrong, and the manual
path is untouched — `sudo bash scripts/update.sh` on the host works exactly as
before. Your previous `.env` and infra files are kept in the install directory
as `.env.backup-<version>` and `.infra-backup-<version>/`.

### Upgrading

**This release cannot be installed with the button it introduces.** Reaching
0.1.8 is one last manual update — `sudo bash scripts/update.sh` from your
install directory. Every release after it can be applied from the dashboard.

No migrations, no changes to how Belune is deployed, no host action.
`update.sh` takes a backup first, as always.

**The image is about 30 MB larger per architecture.** It now carries the
`docker compose` plugin, which the in-app updater needs to reconcile the stack
from inside a container. Nothing else changed in the image's footprint.

## [0.1.7]

### Belune now has a complete API reference

Every endpoint the API exposes is now documented at
[belune.dev/docs/api](https://belune.dev/docs/api) — what it takes, what it
returns, which scope reaches it, and whether it needs the Admin role. 0.1.6
shipped personal access tokens with no reference to use them against; this is
that gap closed.

- **One page per endpoint**, grouped by what it acts on: Account,
  Applications, Databases, Projects, and Platform.
- **Generated from the running server, not written by hand.** Scopes, role
  requirements, and session boundaries are read off the real router, so the
  reference cannot drift from what the code enforces — a build fails if it
  does.
- **The [OpenAPI 3.1 spec](https://belune.dev/openapi.json) is served at a
  stable public URL**, outside authentication. Point a client generator or a
  linter at it directly. It is more complete than the pages: it carries every
  route, session-only ones included.
- **Two guides for the streaming endpoints** — the multiplexed [WebSocket
  hub](https://belune.dev/docs/api/overview/websockets) behind `GET /api/ws`,
  and the seven [Server-Sent Events
  streams](https://belune.dev/docs/api/overview/server-sent-events) you can
  tail with `curl`.

Endpoints that no token can call are deliberately absent from the reference —
a page for something you cannot call from a script is a page for a reader who
cannot act on it. They are described in [What a token can never
do](https://belune.dev/docs/api#what-a-token-can-never-do) instead.

### Members can see which certificate serves their domains

The TLS status table was admin-only as a whole. A Member could read the
certificate serving one domain from that domain's own badge, but had to open
each application in turn to find which of their domains was failing — the
exact thing an at-a-glance table exists to prevent.

- **The Certificates page is now visible to Members**, showing the Domain TLS
  table for the domains they can already reach: their own projects and any
  shared with them. Uploading, replacing and deleting certificates stay
  admin-only, and the page says so.
- **The certificate picker when adding a domain is no longer empty for a
  Member.** It offers the certificates that can actually serve the hostname
  being added, wildcards included, rather than nothing at all.
- **Domains now show which certificate is serving them**, by name, instead of
  leaving the reader to match a bare id.

The certificate store itself stays admin-only. Listing every certificate
returns every hostname on it, which would hand a Member the domain names of
every other application on the instance — so a Member is given the
certificates that match their own hostname, never the full list.

### Sharing or transferring a project now requires a dashboard session

`PUT /api/projects/{id}/sharing` and `PUT /api/projects/{id}/transfer` no
longer accept a personal access token at any scope, joining [everything else a
token can never do](https://belune.dev/docs/api#what-a-token-can-never-do).
Transferring also now requires the Admin role at the route itself, which is
where the rest of the admin-only routes already declare it.

**Why this one matters more than its size suggests.** Sharing a project grants
every Member on the install the owner's working access to it — including
revealing environment variable values, webhook secrets, deploy-hook tokens,
and file mount contents. A single `{"shared": true}` from a leaked token
exposed all of it. Transfer is the targeted version: it hands one named user
owner access, removes the previous owner, who cannot undo it without an admin,
and moves quota accounting and alert recipients with it.

Both flags can be switched back. The secrets they expose in the meantime
cannot be un-exposed, which is why they belong behind a session rather than
behind a scope.

**Upgrading from 0.1.6: breaking.** Both were `write`-scope token-callable
there. A script that shares or transfers projects needs a session credential.

### Personal access tokens can no longer manage the account itself

A PAT manages infrastructure — deploying, editing environment variables,
running backups — not the account it belongs to. The following now require
a live dashboard session, joining [deleting or restoring data, reading a
stored secret, and minting or revoking tokens, which already
did](https://belune.dev/docs/api#what-a-token-can-never-do):

- Changing your password or profile, and logging out.
- Enrolling, disabling, or checking the status of two-factor authentication,
  and regenerating recovery codes.
- **Listing your own personal access tokens.** The list includes every
  token's scopes and last-used time — read-only, but still a credential
  inventory a leaked token could use to find the one that holds `write`.

Notification preferences were considered and left alone: they're
infrastructure config (deployment failures, resource thresholds — the same
things a token already manages), not account security, so a token can still
read and change them.

None of this closes an account-takeover path: password changes, disabling
TOTP, and regenerating recovery codes already required your current
password (and a current second factor, where one is enrolled) before this.
It's a narrower surface for a leaked token to see, not a vulnerability fix.

**Upgrading from 0.1.6:** all three bullets above were PAT-callable there. If
you have a script that changes its own password, manages TOTP, or lists its
own tokens using a personal access token, it will start getting `403`
instead of `200` — point it at a session credential instead, or drop the
call if it isn't essential. Notification-preference calls are unaffected.

### Platform configuration now requires a dashboard session

Reading or changing platform settings, restarting a service, or opening a
host shell now requires a live dashboard session — a personal access token
is rejected outright, joining [everything else a token can never
do](https://belune.dev/docs/api#what-a-token-can-never-do). Gated:
listing and updating instance settings, the SMTP configuration, restarting a
service, and starting a host shell session. `GET /api/maintenance/server-ip`
stays token-callable — a public fact, not configuration.

**The reason this matters: the settings endpoint writes any key by name.** A
handful of keys are validated (the dashboard's own domain and TLS mode, the
backup schedule, the server IP override); everything else passes straight
through to storage. `host_shell_enabled` — the flag that turns on the in-UI
host shell — is one of those keys. Before this, an admin's write-scoped
token could flip that flag with nobody at a keyboard; opening a session from
there still required the password and a second factor, but a token should
never have been able to reach the switch at all. The same endpoint also sets
the dashboard's own domain and TLS mode — where Caddy gets its certificate
from.

The SMTP and settings-listing endpoints are gated alongside it for
consistency, not because either leaks a credential: the SMTP password was
already masked to a presence flag on read, and the settings list already
skipped it. Reading them is still configuration disclosure and
security-posture reconnaissance — the host shell flag, the dashboard's
domain and TLS mode, the public IP override, the backup schedule — that a
leaked token shouldn't get for free, so they move with the write.

**Upgrading from 0.1.6: breaking.** If you have a script that reads or
writes instance settings, manages the SMTP config, restarts a service, or
opens a host shell using a personal access token, it will start getting
`403` instead of `200` — point it at a session credential instead, or drop
the call if it isn't essential. `GET /api/maintenance/server-ip` is
unaffected.

### Deleting a TLS certificate no longer requires a dashboard session

This loosens a boundary that shipped in 0.1.6. `DELETE /api/certificates/{id}`
was session-only — one of the things [a token could never
do](https://belune.dev/docs/api#what-a-token-can-never-do). It now
also accepts a personal access token that has `write` scope **and** belongs
to an Admin, the same bar as every other write in the platform-admin route
group.

Why this is a narrow relaxation and not a hole:

- `domains.certificate_id` is `ON DELETE RESTRICT`, so a certificate any
  domain still serves cannot be deleted at all — only an unused one is
  reachable.
- The route still requires the Admin role, so a leaked Member token, or any
  token without `write`, still gets `403`.
- A deleted certificate is re-uploadable from the same certificate and key
  that created it — unlike a dropped database, nothing is lost for good.

**Upgrading from 0.1.6:** nothing breaks — this only widens what a token is
allowed to do. If you audit the token boundary, this is the one place it has
moved outward.

### Some API paths have moved

Four paths changed. All four kept their scope and role requirements — the path
is the only thing that moved.

| Was                         | Is now                                  |
| --------------------------- | --------------------------------------- |
| `GET /api/metrics`          | `GET /api/summary`                      |
| `POST /api/cleanup`         | `POST /api/maintenance/cleanup`         |
| `GET /api/proxy/reconciler` | `GET /api/maintenance/proxy`            |
| `POST /api/proxy/reconcile` | `POST /api/maintenance/proxy/reconcile` |

`/api/metrics` returned resource **counts** — how many projects, applications,
databases and deployments exist — while `/api/metrics/*` held real host
time-series and `/metrics` was the Prometheus scrape endpoint. Three unrelated
meanings on one prefix. `/api/metrics/*` now holds only genuine series.

The other three were already described as maintenance operations while living
outside `/api/maintenance`. Note the status endpoint is
`/api/maintenance/proxy`, not `.../proxy/reconciler`: `GET` the noun for
status, `POST` the noun plus a verb for the action, matching
`/api/maintenance/queue` and `/api/maintenance/queue/clear`.

**Upgrading from 0.1.6: breaking, and worth grepping for.** These are the only
paths that have moved since 0.1.6, and they moved now rather than later
precisely because the API reference had not been published yet — no document
had promised them. After this release they are a published contract and will
not move again without a deprecation period. The dashboard is unaffected; it
ships with the binary and already calls the new paths.

### Fixed

- **Deleting an application destroyed its volume backups without saying so.**
  The dialog warned only that the container would stop and the application
  would be deleted. It also erases every volume backup the application had,
  local files and remote objects alike, with no way to keep them — unlike
  deleting a database, which offers that choice. The dialog now states it. The
  behaviour is unchanged; what changes is that you are told before you confirm.
- **An unknown `/api/` path returned `200` and an HTML page instead of a
  `404`.** Anything the API did not recognise fell through to the dashboard's
  own catch-all, which answers unknown paths with the app itself so that
  in-browser navigation works. A caller that typo'd an endpoint got a success
  status and a web page, which most clients fail to parse in some confusing
  way rather than reporting "not found". Unmatched `/api/` paths now return
  `404` with the same `{"error": "..."}` shape as every other API failure;
  dashboard links and page refreshes are unchanged.
- **The settings endpoint accepted any key you sent it.** Seven keys were
  validated and the rest were written as-is, so `host_shel_enabled` — one
  character off the host-shell gate — returned `200`, created a real row, read
  back correctly afterwards, and turned nothing on. `PUT /api/settings` now
  accepts only the 16 keys Belune actually reads, rejecting anything else with
  a `400` that names what it will take, and type-checks the nine keys that
  previously had no validation at all. This is only reachable by an admin with
  a dashboard session, so it is a correctness fix rather than a security one.

### Upgrading

This release adds **no migrations** and changes nothing about how Belune is
deployed — no new containers, no compose changes, no host action. `update.sh`
takes a backup first, as always.

**The dashboard is entirely unaffected.** It authenticates with a session
cookie, so every boundary change above is invisible to it. Nothing you do in
the browser changes.

**If you have a script using a personal access token, read the breaking
entries above.** In short: point anything that manages the account, reads or
writes platform configuration, or shares or transfers a project at a session
credential instead, and update the four moved paths. Everything else keeps
working unchanged.

## [0.1.6]

### Projects can now be shared with your team

**Two Members could not collaborate on a project before this.** A project had
one owner and nothing else, so a team reached for the only lever available:
promote everyone to Admin just to let them work together. A project owner can
now share a project with every Member on the install instead.

- A single switch on the project's Settings tab, off by default — nothing
  changes for an existing project until its owner turns it on.
- A shared Member gets full working access: deploy, create databases, edit
  environment variables, all of it.
- **Destructive rights stay owner-only.** Deleting an application, database,
  or domain, transferring the project, and unsharing it all still require
  being the owner or an admin — sharing widens who can use a project, not who
  can destroy it.
- A Member who owns a project can share it themselves; it doesn't need an
  admin.

### Personal access tokens

Belune now has API credentials that aren't your login. Create one from
**Account → Personal Access Tokens**, [documented here](https://belune.dev/docs/api):

- **Four scopes on a ladder** — `metrics` ⊂ `read` ⊂ `deploy` ⊂ `write` —
  each including everything narrower than it, so a token only ever needs one
  rung. A monitoring scraper wants `metrics`; a CI job that only deploys
  wants `deploy`.
- **Expiry: 1, 7, 14, 30, 60, or 90 days, or never** — 30 days by default.
- Shown exactly once at creation. There's no "regenerate" — create a new
  token, confirm it works, then delete the old one, so there's never a gap
  where nothing has access.
- The token list shows when each one was last used, so one nobody's touched
  in months is easy to spot and retire.
- Every audit-log entry now records which token performed an action, or that
  it was you directly.

By design, **no token can ever delete or restore anything, read a stored
secret** (database credentials, deploy-hook tokens, webhook secrets,
environment variables, file mount contents), **mint or revoke a token,
manage users, or open a terminal** — those always require a live dashboard
session, whatever scope the token carries. This isn't a capability being
taken away — personal access tokens are new in this release, so there's
nothing that previously worked with one. It's the ceiling the new credential
ships with: a leaked token is a bad day, not a catastrophe.

### Changed

- **Resetting another user's password now asks for the acting admin's own
  password first.** Resetting another user's two-factor authentication asks
  for the admin's password, and a fresh code too if the admin has 2FA
  enrolled themselves. Both show up as a new prompt on the Team page —
  expect it, it isn't a bug.
- **`GET /api/projects/{id}/databases/{id}` no longer returns connection
  credentials.** They live at a new endpoint,
  `.../databases/{id}/credentials/reveal`, which — like every endpoint that
  hands back a decrypted secret — requires a live session. The dashboard
  already calls the new endpoint; anyone who was reading the `credentials` or
  `connection_string` field off the old response, with a session cookie,
  needs to switch to it.

### Upgrading

This release adds two migrations (project sharing, personal access tokens).
`update.sh` takes a backup first, as always. Both are purely additive —
sharing defaults to off for every existing project, and there is nothing to
migrate for a credential type that didn't exist before.

## [0.1.5]

### Backups now outlive the database they came from

**Deleting a database no longer destroys its backups.** Until now it did, and
v0.1.3 only made that consented rather than silent — but consent to an
irreversible mistake is still irreversible, and the moment you want yesterday's
backup is right after deleting the database by accident.

- The delete dialog offers **"Also delete these backups"**, unchecked. The API
  mirrors it: backups are kept unless `delete_backups=true`, so a script or an
  older client keeps the data rather than destroying it.
- Kept backups appear under the project's **Backups** page, in a new section for
  backups whose database is gone. They would otherwise be invisible — the
  per-database page went with the database — while still costing storage.
- **Restore a replacement** from any of them. The database comes back under its
  **original name and credentials**, so applications in the project reconnect
  with no configuration change. This matters more than it looks: attaching a
  database injects no connection variables, so a replacement under a new name
  would leave every dependent application pointing at a host that is not there.
- Kept backups **expire 90 days after the database was deleted**, so keeping by
  default cannot quietly grow remote storage forever. Change it with the
  `orphaned_backup_retention_days` setting, or set it to `0` to keep everything
  and decide by hand.

**Deleting a project still destroys everything in it**, including kept backups —
unchanged, and now stated more fully in its confirmation dialog.

### Fixed

- **Deleting an application left its volume backups behind.** The database rows
  cascaded away with the volumes, but the archives stayed in their destination
  with nothing left recording where they were: unreachable, unprunable, and
  still billed. They are now erased with the application. If you have deleted
  applications that had volume backups, objects from before this release are
  still in your destination and need removing by hand — Belune no longer has a
  record of their keys.
- The daily orphan-container sweep now identifies containers by the labels they
  carry rather than by name, so a container whose name has changed is no longer
  invisible to it, and managed databases are covered structurally rather than by
  a list someone has to remember to update. It also sweeps each server against
  what is placed on it, rather than one host against every row.
- Live log, metric and notification streams no longer die with a nil-pointer
  panic when their Redis subscription ends — which happens on any ordinary API
  restart while a stream is open.

### Upgrading

This release adds a migration. `update.sh` takes a backup first, as always.
Nothing existing is rewritten: the new columns are empty for every backup you
already have, and every one of them keeps reading exactly as it did before.

## [0.1.0] — first public release

The first release published to GHCR and the first version installable with the
one-line installer. Everything below already existed across 36 alpha
iterations; this is what Belune _is_ at launch, not a list of what changed.

### Deploying applications

- Deploy from a **git repository** — GitHub, GitLab, Gitea, or Bitbucket — via
  GitHub App installations, OAuth connections, or a personal access token.
- Builds with **Railpack** (default), **Cloud Native Buildpacks**, or your own
  **Dockerfile**, with a persistent build cache per application.
- Deploy from a **prebuilt image**, with the digest pinned so a redeploy is
  reproducible.
- **Automatic deploys**: push webhooks with per-branch filtering, and per-app
  **deploy hooks** — a tokenised URL your CI can call after publishing an image.
- Staged deploys with **health verification and automatic rollback**: the new
  image is built before the running container is replaced, and a failed health
  check reverts to the previous deployment.
- Rollback to any previous deployment, and per-deployment build logs.

### One-click apps

- A catalog of **app templates** that instantiate native Belune objects —
  applications, managed databases, volumes, environment variables, and a domain
  — so a templated app gets the same backups, upgrades, and observability as
  anything else.
- Templates are declarative manifests; adding one needs no Go or React.

### Managed databases

- **PostgreSQL, MySQL, MongoDB and Redis**, plus a generic "other" type for any
  database image.
- **Scheduled backups** to S3-compatible storage with retention, plus manual
  backups and in-app restore.
- **Guarded major-version upgrades**: dump, verify, migrate, and roll back
  automatically if anything fails.
- Optional external access over a loopback-bound port for SSH tunnelling.

### Networking and TLS

- **Automatic HTTPS** via Caddy and Let's Encrypt, plus upload of custom
  certificates (Cloudflare Origin CA and friends).
- **Per-domain TLS status** — issued, expiring, failed — with the actual reason
  for a failure, including DNS misconfiguration detected before issuance is
  attempted.
- Path-based routing, HTTP→HTTPS redirects, and per-project network isolation.

### Storage

- **Volumes** and **file mounts** with content managed in Belune.
- Volume **snapshot backups** to S3 on a schedule, with restore.

### Operations

- Per-container **logs** grouped into deployment sessions, with severity levels
  and search; platform logs for Belune's own services.
- **Metrics** for host and containers, with history.
- A browser **terminal** into any application container.
- **Notifications** to Discord, Telegram, Slack, ntfy, Gotify, generic webhooks,
  or email when deploys, backups, restores, or certificates need attention.
- **Audit log**, per-user quotas, role-based access, disk cleanup, and
  read-only Docker views.
- System backup and documented disaster recovery.

### Security

- Envelope encryption with key rotation for all secrets at rest.
- Refresh tokens, login lockout, session revocation, CSRF protection.
- Containers run with dropped capabilities, no new privileges, and a read-only
  root filesystem by default.
- Optional, off-by-default host shell for break-glass recovery — admin-only,
  re-authenticated, and fully audited.

### Known limitations at 0.1.0

- **Single server.** Multi-server support is planned but not present.
- **No Docker Compose import.** Templates cover common stacks natively; compose
  import is on the roadmap.
- **Preview environments** exist but are unlinked in the UI pending completion.
- Registry credentials for private images, monorepo subdirectory builds, and
  custom start commands are not yet configurable.

[0.1.11]: https://github.com/weiliang79/belune/releases/tag/v0.1.11
[0.1.10]: https://github.com/weiliang79/belune/releases/tag/v0.1.10
[0.1.9]: https://github.com/weiliang79/belune/releases/tag/v0.1.9
[0.1.8]: https://github.com/weiliang79/belune/releases/tag/v0.1.8
[0.1.7]: https://github.com/weiliang79/belune/releases/tag/v0.1.7
[0.1.6]: https://github.com/weiliang79/belune/releases/tag/v0.1.6
[0.1.5]: https://github.com/weiliang79/belune/releases/tag/v0.1.5
[0.1.0]: https://github.com/weiliang79/belune/releases/tag/v0.1.0
