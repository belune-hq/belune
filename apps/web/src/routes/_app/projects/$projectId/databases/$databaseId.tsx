import { createFileRoute, useNavigate } from "@tanstack/react-router";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { z } from "zod";
import { ContainerLogViewer } from "@/components/logs/container-log-viewer";
import { BackupConfigFormDialog } from "@/components/databases/backup-config-form-dialog";
import { BackupConfigRunsSheet } from "@/components/databases/backup-config-runs-sheet";
import {
  useDatabaseBackupConfigs,
  useRunBackupConfig,
  useDeleteBackupConfig,
} from "@/lib/hooks/use-database-backup-configs";
import { useBackupDestinations } from "@/lib/hooks/use-backup-destinations";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { toast } from "sonner";
import {
  useDatabase,
  useDatabaseVolume,
  useDatabaseCredentials,
  useDatabaseDeletionImpact,
  useDeleteDatabase,
  useSetDatabaseResources,
  useSetDatabaseExternalAccess,
  useDatabaseBackups,
  useBackupDatabase,
  useUpgradeDatabase,
  useStopDatabase,
  useStartDatabase,
  useRestartDatabase,
  useReloadDatabase,
} from "@/lib/hooks/use-databases";
import { useProject } from "@/lib/hooks/use-projects";
import { useAuthStore } from "@/lib/stores/auth";
import {
  Database as DatabaseIcon,
  Loader2,
  Trash2,
  Pencil,
  History,
  TriangleAlert as AlertTriangleIcon,
  DatabaseBackup as DatabaseBackupIcon,
  Plus,
  LayoutDashboard,
  ScrollText,
  Archive,
  Settings,
  RotateCcwIcon,
  RefreshCwIcon,
  SquareIcon,
  PlayIcon,
} from "lucide-react";
import { ButtonGroup } from "@/components/ui/button-group";
import { IconAction } from "@/components/ui/icon-action";
import { PageTabs, type PageTab } from "@/components/ui/page-tabs";
import { Skeleton } from "@/components/ui/skeleton";
import { useBreadcrumbLabel } from "@/lib/hooks/use-breadcrumb";
import { StatusBadge } from "@/lib/components/status-badge";
import { DatabaseReloadBadge } from "@/lib/components/database-reload-badge";
import { ProvenanceNote } from "@/lib/components/provenance-note";
import { CopyButton } from "@/lib/components/copy-button";
import { formatBytes, formatList } from "@/lib/utils/format";
import { cn } from "@/lib/utils";
import type {
  Database,
  DatabaseBackupConfig,
  DatabaseCredentials,
} from "@/lib/types";
import {
  Field,
  FieldContent,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@/components/ui/field";
import { fieldError } from "@/lib/utils/field-error";
import { RouteError } from "@/lib/components/route-error";

// Engines with an in-image logical-dump tool (pg_dump/mysqldump/mongodump).
// redis (cache) has no logical backup. "other" is backed up when a backup mode
// (volume snapshot or custom commands) was configured at creation.
const BACKUP_SUPPORTED_TYPES = ["postgres", "mysql", "mongo"];

function dbBackupEnabled(db: Database): boolean {
  if (db.type === "other") return db.backup_mode !== "none";
  return BACKUP_SUPPORTED_TYPES.includes(db.type);
}

// imageLabel shows the engine:tag for known engines and the full image for "other".
function imageLabel(db: Database): string {
  return db.type === "other" ? (db.image ?? "—") : `${db.type}:${db.version}`;
}

// Engines eligible for the guarded major-version upgrade (logical dump-and-reload).
function dbUpgradable(db: Database): boolean {
  return db.type === "postgres" || db.type === "mysql" || db.type === "mongo";
}

type DbTab = "overview" | "logs" | "backups" | "settings";

const DB_TABS: PageTab<DbTab>[] = [
  { value: "overview", label: "Overview", icon: LayoutDashboard },
  { value: "logs", label: "Logs", icon: ScrollText },
  { value: "backups", label: "Backups", icon: Archive },
  { value: "settings", label: "Settings", icon: Settings },
];

export const Route = createFileRoute(
  "/_app/projects/$projectId/databases/$databaseId",
)({
  component: DatabaseDetailPage,
  errorComponent: RouteError,
  validateSearch: (search: Record<string, unknown>): { tab?: DbTab } =>
    search.tab === "logs" ||
    search.tab === "backups" ||
    search.tab === "settings"
      ? { tab: search.tab as DbTab }
      : {},
});

function DatabaseDetailPage() {
  const { projectId, databaseId } = Route.useParams();
  const { tab } = Route.useSearch();
  const navigate = useNavigate();
  const { data: db, isLoading } = useDatabase(projectId, databaseId);
  // Only fetched once the database is actually running — matches when the
  // Connection Details card that needs it becomes visible.
  const { data: creds } = useDatabaseCredentials(projectId, databaseId, {
    enabled: db?.status === "running",
  });
  const { data: project } = useProject(projectId);
  const currentUser = useAuthStore((s) => s.user);
  const canDelete =
    currentUser?.role === "admin" || currentUser?.id === project?.user_id;
  const deleteDb = useDeleteDatabase(projectId);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const deleteForm = useForm({
    // Unchecked by default: deleting a database is recoverable from a
    // backup, deleting the backups with it is not.
    defaultValues: { confirm: "", deleteBackups: false },
    onSubmit: ({ value }) => {
      toast.promise(
        deleteDb
          .mutateAsync({ databaseId, deleteBackups: value.deleteBackups })
          .then(() => {
            navigate({ to: "/projects/$projectId", params: { projectId } });
          }),
        {
          loading: "Deleting database...",
          success: "Database deleted",
          error: (err) => err.message,
        },
      );
    },
  });
  const { data: deleteImpact } = useDatabaseDeletionImpact(
    projectId,
    databaseId,
    deleteOpen,
  );
  const stop = useStopDatabase(projectId, databaseId);
  const start = useStartDatabase(projectId, databaseId);
  const restart = useRestartDatabase(projectId, databaseId);
  const reload = useReloadDatabase(projectId, databaseId);

  const activeTab: DbTab = tab ?? "overview";
  const setTab = (next: DbTab) =>
    navigate({
      to: ".",
      search: () => ({ tab: next === "overview" ? undefined : next }),
      replace: true,
    });

  useBreadcrumbLabel(projectId, project?.name);
  useBreadcrumbLabel(databaseId, db?.name);

  if (isLoading || !db) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-4 w-64" />
        <div>
          <Skeleton className="h-7 w-48" />
          <Skeleton className="mt-1 h-4 w-24" />
        </div>
        <Card>
          <CardHeader>
            <Skeleton className="h-6 w-40" />
          </CardHeader>
          <CardContent className="space-y-3">
            {[1, 2, 3].map((i) => (
              <Skeleton key={i} className="h-5 w-full" />
            ))}
          </CardContent>
        </Card>
      </div>
    );
  }

  // Transitional states own their container via a running task; Reload must not
  // race them. Restart/Start already guard on these separately.
  const transitional =
    db.status === "creating" ||
    db.status === "upgrading" ||
    db.status === "backing_up";

  const handleReload = () => {
    toast.promise(reload.mutateAsync(), {
      loading: "Reloading…",
      success: "Reload started — recreating the container",
      error: (err) => err.message,
    });
  };

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <div className="bg-elev text-text-muted grid size-11 shrink-0 place-items-center rounded-xl">
            <DatabaseIcon aria-hidden="true" className="size-5" />
          </div>
          <div className="min-w-0">
            <div className="flex items-center gap-2.5">
              <h1 className="truncate text-2xl font-semibold tracking-tight">
                {db.name}
              </h1>
              <Badge variant="outline" className="font-mono">
                {imageLabel(db)}
              </Badge>
              <StatusBadge status={db.status} />
              <DatabaseReloadBadge db={db} />
            </div>
            <p className="text-text-faint flex flex-wrap items-center gap-x-2 truncate text-sm">
              <span className="truncate font-mono">{db.slug}</span>
              <ProvenanceNote
                sourceKind={db.source_kind}
                sourceRef={db.source_ref}
              />
            </p>
          </div>
        </div>
        {/* One segmented cluster of lifecycle actions, mirroring the App
            Details header. Restart stops/starts the existing container; Reload
            recreates it from the record (recovering a drifted or deleted
            container); Stop/Start toggles the current one. */}
        <ButtonGroup aria-label="Database actions">
          <Button
            size="sm"
            variant="outline"
            onClick={() => {
              toast.promise(restart.mutateAsync(), {
                loading: "Restarting...",
                success: "Database restarted",
                error: (err) => err.message,
              });
            }}
            disabled={restart.isPending || db.status !== "running"}
            title="Restart the existing container in place"
          >
            <RotateCcwIcon aria-hidden="true" />
            Restart
          </Button>
          <Button
            size="sm"
            variant="outline"
            onClick={handleReload}
            disabled={reload.isPending || transitional}
            title="Recreate the container from the stored configuration — recovers a missing or drifted container. The data volume is preserved."
          >
            <RefreshCwIcon aria-hidden="true" />
            Reload
          </Button>
          {db.status === "running" ? (
            <AlertDialog>
              <AlertDialogTrigger
                render={
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={stop.isPending}
                  />
                }
              >
                <SquareIcon aria-hidden="true" />
                {stop.isPending ? "Stopping..." : "Stop"}
              </AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>Stop {db.name}?</AlertDialogTitle>
                  <AlertDialogDescription>
                    This will stop the running container. You can start it again
                    later. Connected services will lose access until it&apos;s
                    running again.
                  </AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel>Cancel</AlertDialogCancel>
                  <AlertDialogAction
                    onClick={() => {
                      toast.promise(stop.mutateAsync(), {
                        loading: "Stopping...",
                        success: "Database stopped",
                        error: (err) => err.message,
                      });
                    }}
                  >
                    Stop
                  </AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>
          ) : (
            <Button
              size="sm"
              variant="outline"
              onClick={() => {
                toast.promise(start.mutateAsync(), {
                  loading: "Starting...",
                  success: "Database started",
                  error: (err) => err.message,
                });
              }}
              disabled={start.isPending || transitional}
            >
              <PlayIcon aria-hidden="true" />
              {start.isPending ? "Starting..." : "Start"}
            </Button>
          )}
        </ButtonGroup>
      </div>

      {/* The container was deleted out from under us (drift), but the record and
          data volume are intact. Restart/Start can't recover this — only a
          recreate can — so nudge Reload contextually, in addition to the always-
          available header button. */}
      {db.container_missing && !transitional && (
        <Card className="bg-status-error-soft ring-status-error-line">
          <CardContent className="flex flex-wrap items-center justify-between gap-3 py-4">
            <div className="flex min-w-0 items-start gap-3">
              <AlertTriangleIcon
                aria-hidden="true"
                className="text-destructive mt-0.5 size-4 shrink-0"
              />
              <div className="min-w-0 space-y-1">
                <p className="text-sm font-medium">
                  The database container is missing
                </p>
                <p className="text-muted-foreground text-sm">
                  Its container was removed from the host, but the database and
                  its data volume are intact. Reload to recreate the container
                  from the stored configuration.
                </p>
              </div>
            </div>
            <Button
              size="sm"
              onClick={handleReload}
              disabled={reload.isPending}
              className="shrink-0"
              variant="destructive-solid"
            >
              <RefreshCwIcon aria-hidden="true" className="mr-1 size-4" />
              Reload
            </Button>
          </CardContent>
        </Card>
      )}

      {(db.status === "creating" ||
        db.status === "upgrading" ||
        db.status === "backing_up") && (
        <Card>
          <CardContent className="flex items-center gap-3 py-6">
            <Loader2 className="h-5 w-5 animate-spin" />
            <div>
              <p className="font-medium">
                {db.status === "upgrading"
                  ? "Upgrading database…"
                  : db.status === "backing_up"
                    ? "Backing up database…"
                    : "Provisioning database..."}
              </p>
              <p className="text-muted-foreground text-sm">
                {db.status === "upgrading"
                  ? "Dumping, rebuilding at the new version, and restoring. The database is briefly offline."
                  : db.status === "backing_up"
                    ? "Briefly offline while a volume snapshot is taken."
                    : "This usually takes a few seconds."}
              </p>
            </div>
          </CardContent>
        </Card>
      )}

      {db.status === "failed" && (
        <Card>
          <CardContent className="space-y-1 py-6">
            <p className="text-destructive text-sm font-medium">
              This database is in a failed state.
            </p>
            <p className="text-muted-foreground text-sm">
              A failed provision or reconfigure usually leaves the data volume
              intact. If an upgrade was interrupted, recover from the most
              recent pre-upgrade backup. Otherwise remove the database from the
              Danger Zone (Settings tab) if you no longer need it.
            </p>
          </CardContent>
        </Card>
      )}

      <PageTabs
        ariaLabel="Database views"
        items={DB_TABS}
        value={activeTab}
        onValueChange={setTab}
      />

      {activeTab === "overview" && (
        <div className="space-y-6">
          {db.status === "running" && creds?.credentials ? (
            <Card>
              <CardHeader>
                <CardTitle>Connection Details</CardTitle>
                <CardDescription>
                  Use these credentials to connect from your services on the
                  internal network.
                </CardDescription>
              </CardHeader>
              <CardContent className="space-y-4">
                <div className="space-y-3">
                  <div className="flex items-center justify-between text-sm">
                    <span className="text-muted-foreground">Host</span>
                    <div className="flex items-center gap-1">
                      <code className="text-xs">{db.internal_host}</code>
                      <CopyButton value={db.internal_host} />
                    </div>
                  </div>
                  <div className="flex items-center justify-between text-sm">
                    <span className="text-muted-foreground">Port</span>
                    <div className="flex items-center gap-1">
                      <code className="text-xs">{db.internal_port}</code>
                      <CopyButton value={String(db.internal_port)} />
                    </div>
                  </div>
                  <Separator />
                  {Object.entries(creds.credentials).map(([key, value]) => (
                    <div
                      key={key}
                      className="flex items-center justify-between text-sm"
                    >
                      <span className="text-muted-foreground">{key}</span>
                      <div className="flex items-center gap-1">
                        <code className="text-xs">{value}</code>
                        <CopyButton value={value} />
                      </div>
                    </div>
                  ))}
                </div>

                {creds.connection_string && (
                  <>
                    <Separator />
                    <div className="space-y-2">
                      <p className="text-muted-foreground text-sm">
                        Connection String
                      </p>
                      <div className="bg-muted flex items-center justify-between rounded-md px-3 py-2">
                        <code className="text-xs break-all">
                          {creds.connection_string}
                        </code>
                        <CopyButton value={creds.connection_string} />
                      </div>
                    </div>
                  </>
                )}
              </CardContent>
            </Card>
          ) : (
            db.status !== "failed" && (
              <Card>
                <CardContent className="text-muted-foreground py-6 text-sm">
                  Connection details appear once the database is running.
                </CardContent>
              </Card>
            )
          )}

          {db.status === "running" && (
            <ExternalAccessCard db={db} creds={creds} />
          )}
        </div>
      )}

      {activeTab === "logs" && (
        <ContainerLogViewer
          source="database"
          projectId={projectId}
          sourceId={databaseId}
        />
      )}

      {activeTab === "backups" && (
        <div className="space-y-6">
          {db.status === "running" && dbBackupEnabled(db) ? (
            <BackupsTab db={db} canDelete={canDelete} />
          ) : (
            <Card>
              <CardContent className="text-muted-foreground py-6 text-sm">
                {dbBackupEnabled(db)
                  ? "Backups are available once the database is running."
                  : "Backups are not supported for this database type."}
              </CardContent>
            </Card>
          )}
        </div>
      )}

      {activeTab === "settings" && (
        <div className="space-y-6">
          {db.status === "running" && <AdvancedCard db={db} />}

          {canDelete && (
            <>
              {/* ring-, not border-: Card draws its edge with `ring-1`, and Tailwind
              preflight zeroes border-width, so a `border-*` colour never renders.
              bg/ring use the status-error soft/line tokens so both themes get a
              red chosen for them. Mirrors the App Details Danger Zone. */}
              <Card className="bg-status-error-soft ring-status-error-line">
                <CardHeader>
                  <CardTitle className="text-destructive flex items-center gap-2">
                    <AlertTriangleIcon aria-hidden="true" className="size-4" />
                    Danger Zone
                  </CardTitle>
                </CardHeader>
                <CardContent>
                  <div className="flex flex-wrap items-center justify-between gap-3">
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <Trash2
                          aria-hidden="true"
                          className="text-destructive size-4"
                        />
                        <p className="text-sm font-medium">
                          Delete this database
                        </p>
                      </div>
                      <p className="text-muted-foreground mt-1 text-xs">
                        This will permanently delete the database, its data, its
                        container, and every backup taken of it.
                      </p>
                    </div>
                    <AlertDialog
                      open={deleteOpen}
                      onOpenChange={(o) => {
                        setDeleteOpen(o);
                        // Reset on open, so a previous tick or half-typed name can
                        // never carry into a later, different decision.
                        if (o) deleteForm.reset();
                      }}
                    >
                      <AlertDialogTrigger
                        render={
                          <Button variant="destructive-solid" size="sm" />
                        }
                      >
                        Delete
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <form
                          onSubmit={(e) => {
                            e.preventDefault();
                            e.stopPropagation();
                            deleteForm.handleSubmit();
                          }}
                          className="space-y-4"
                        >
                          <AlertDialogHeader>
                            <AlertDialogTitle>
                              Delete database?
                            </AlertDialogTitle>
                            <AlertDialogDescription>
                              This will permanently delete &quot;{db.name}&quot;
                              and all its data. This action cannot be undone.
                            </AlertDialogDescription>
                            {deleteImpact && deleteImpact.backup_count > 0 ? (
                              <AlertDialogDescription>
                                Its{" "}
                                {deleteImpact.backup_count === 1
                                  ? "1 backup is"
                                  : `${deleteImpact.backup_count} backups are`}{" "}
                                kept
                                {deleteImpact.backup_destinations.length > 0
                                  ? `, including copies in ${formatList(deleteImpact.backup_destinations)}`
                                  : ""}
                                , and stay listed under the project&apos;s
                                Backups tab. You can restore a replacement
                                database from them.
                              </AlertDialogDescription>
                            ) : null}
                          </AlertDialogHeader>
                          {deleteImpact && deleteImpact.backup_count > 0 ? (
                            <deleteForm.Field
                              name="deleteBackups"
                              children={(field) => (
                                <Field orientation="horizontal">
                                  <Checkbox
                                    id="delete-db-backups"
                                    checked={field.state.value}
                                    onCheckedChange={(checked) =>
                                      field.handleChange(checked === true)
                                    }
                                    className="mt-0.5"
                                  />

                                  <FieldContent>
                                    <FieldLabel
                                      htmlFor="delete-db-backups"
                                      className="leading-snug font-normal"
                                    >
                                      Also delete{" "}
                                      {deleteImpact.backup_count === 1
                                        ? "this backup"
                                        : "these backups"}
                                    </FieldLabel>

                                    <FieldDescription>
                                      {deleteImpact.backup_destinations.length >
                                      0
                                        ? "Erases the archives, including the remote copies. This cannot be undone."
                                        : "Erases the archives. This cannot be undone."}
                                    </FieldDescription>
                                  </FieldContent>
                                </Field>
                              )}
                            />
                          ) : null}
                          <deleteForm.Field
                            name="confirm"
                            validators={{
                              onChange: ({ value }) =>
                                value.trim() !== db.name.trim()
                                  ? "Does not match"
                                  : undefined,
                            }}
                            children={(field) => (
                              <div className="space-y-2">
                                <Label
                                  htmlFor="delete-db-confirm"
                                  className="font-normal"
                                >
                                  Type{" "}
                                  <span className="text-foreground font-medium">
                                    {db.name}
                                  </span>{" "}
                                  to confirm.
                                </Label>
                                <Input
                                  id="delete-db-confirm"
                                  value={field.state.value}
                                  onBlur={field.handleBlur}
                                  onChange={(e) =>
                                    field.handleChange(e.target.value)
                                  }
                                  autoComplete="off"
                                  autoCorrect="off"
                                  spellCheck={false}
                                />
                              </div>
                            )}
                          />
                          <AlertDialogFooter>
                            <AlertDialogCancel type="button">
                              Cancel
                            </AlertDialogCancel>
                            <deleteForm.Subscribe
                              selector={(s) => s.values.confirm}
                              children={(confirm) => (
                                <AlertDialogAction
                                  type="submit"
                                  disabled={
                                    confirm.trim() !== db.name.trim() ||
                                    deleteDb.isPending
                                  }
                                  className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
                                >
                                  {deleteDb.isPending ? (
                                    <Loader2 className="mr-1 h-4 w-4 animate-spin" />
                                  ) : null}
                                  Delete
                                </AlertDialogAction>
                              )}
                            />
                          </AlertDialogFooter>
                        </form>
                      </AlertDialogContent>
                    </AlertDialog>
                  </div>
                </CardContent>
              </Card>
            </>
          )}
        </div>
      )}
    </div>
  );
}

/** Per-engine localhost connection string reached through the SSH tunnel. */
function localConnectionString(
  db: Database,
  localPort: number,
  credentials: Record<string, string> | undefined,
): string {
  const c = credentials ?? {};
  switch (db.type) {
    case "postgres":
      return `postgresql://${c.user}:${c.password}@localhost:${localPort}/${c.database}`;
    case "mysql":
      return `mysql://${c.user}:${c.password}@localhost:${localPort}/${c.database}`;
    case "redis":
      return `redis://:${c.password}@localhost:${localPort}`;
    case "mongo":
      return `mongodb://${c.username}:${c.password}@localhost:${localPort}`;
    default:
      return "";
  }
}

/**
 * External Access (SSH tunnel). Toggling binds/unbinds a loopback host port,
 * which recreates the container — surfaced via the warning so the brief
 * downtime isn't a surprise. The database is never exposed publicly.
 */
function ExternalAccessCard({
  db,
  creds,
}: {
  db: Database;
  creds: DatabaseCredentials | undefined;
}) {
  const setAccess = useSetDatabaseExternalAccess(db.project_id, db.id);
  const ext = db.external_access;
  const enabled = ext?.enabled ?? false;

  const handleToggle = () => {
    toast.promise(setAccess.mutateAsync(!enabled), {
      loading: enabled
        ? "Disabling external access…"
        : "Enabling external access…",
      success: enabled
        ? "Disabling — recreating container…"
        : "Enabling — recreating container…",
      error: (err) => err.message,
    });
  };

  const localPort = db.internal_port;
  const sshHost = ext?.ssh_host || "<server-host>";
  const sshUser = ext?.ssh_user || "<user>";
  const hostsUnset = !ext?.ssh_host || !ext?.ssh_user;
  // -N: forward the port and nothing else. Without it the command also opens a
  // login shell, which invites the reading that you are "in" the server and
  // leaves people unsure how to end the tunnel. With it, the session is the
  // tunnel: Ctrl-C closes it, and that is obvious.
  const sshCmd = `ssh -N -L ${localPort}:127.0.0.1:${ext?.host_port ?? ""} ${sshUser}@${sshHost}`;
  const connStr = localConnectionString(db, localPort, creds?.credentials);

  return (
    <Card>
      <CardHeader>
        <CardTitle>External Access</CardTitle>
        <CardDescription>
          Reach this database from your machine over an SSH tunnel. It is bound
          to the server&apos;s loopback only — never exposed publicly.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* Toggle + recreate warning */}
        <div className="flex items-center justify-between gap-3">
          <div>
            <p className="text-sm font-medium">Enable external access</p>
            <p className="text-text-faint text-xs">
              Binds a loopback port for SSH tunneling.
            </p>
          </div>
          <button
            type="button"
            role="switch"
            aria-checked={enabled}
            aria-label="Enable external access"
            disabled={setAccess.isPending}
            onClick={handleToggle}
            className={cn(
              "focus-visible:ring-ring relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors focus-visible:ring-2 focus-visible:outline-none disabled:opacity-50",
              enabled ? "bg-primary" : "bg-input",
            )}
          >
            <span
              className={cn(
                "bg-background pointer-events-none inline-block h-4 w-4 rounded-full shadow-lg transition-transform",
                enabled ? "translate-x-4" : "translate-x-0",
              )}
            />
          </button>
        </div>

        <div className="bg-status-building-soft text-status-building ring-status-building-line flex items-start gap-2 rounded-md px-3 py-2 text-xs ring-1 ring-inset">
          <AlertTriangleIcon
            aria-hidden="true"
            className="mt-0.5 size-3.5 shrink-0"
          />
          <span>
            Toggling external access recreates the database container — expect a
            few seconds of downtime. Your data is preserved (the volume is
            reattached).
          </span>
        </div>

        {/* Tunnel details — only when enabled */}
        {enabled && ext?.host_port && (
          <div className="space-y-3 border-t pt-4">
            <div className="space-y-1.5">
              <p className="text-muted-foreground text-xs">
                SSH tunnel command
              </p>
              <div className="bg-muted flex items-center justify-between gap-2 rounded-md px-3 py-2">
                <code className="text-xs break-all">{sshCmd}</code>
                <CopyButton value={sshCmd} />
              </div>
            </div>

            {connStr && (
              <div className="space-y-1.5">
                <p className="text-muted-foreground text-xs">
                  Connection string (via tunnel)
                </p>
                <div className="bg-muted flex items-center justify-between gap-2 rounded-md px-3 py-2">
                  <code className="text-xs break-all">{connStr}</code>
                  <CopyButton value={connStr} />
                </div>
              </div>
            )}

            <p className="text-text-faint text-xs">
              Add <span className="font-mono">-i /path/to/key</span> if the key
              is not in your SSH agent or config. The tunnel lasts as long as
              the command runs — press <span className="font-mono">Ctrl-C</span>{" "}
              to close it.
              {hostsUnset ? (
                <>
                  {" "}
                  Set <span className="font-mono">
                    SERVER_SSH_HOST
                  </span> and <span className="font-mono">SERVER_SSH_USER</span>{" "}
                  to fill the placeholders in automatically.
                </>
              ) : null}
            </p>

            {/* The two ways to connect need different numbers in different boxes,
                and conflating them is what makes this confusing: whether the
                client should be pointed at the local end of the tunnel or the
                server end depends entirely on who opens the tunnel. */}
            <div className="space-y-2 border-t pt-3">
              <p className="text-muted-foreground text-xs">
                In a GUI client (DBeaver, TablePlus, DataGrip)
              </p>
              <dl className="text-text-faint space-y-1 text-xs">
                <div className="flex gap-2">
                  <dt className="w-40 shrink-0">Running the command above</dt>
                  <dd className="font-mono">
                    connect to localhost:{localPort}
                  </dd>
                </div>
                <div className="flex gap-2">
                  <dt className="w-40 shrink-0">
                    Using the client&apos;s own SSH tunnel
                  </dt>
                  <dd className="min-w-0">
                    <span className="font-mono">
                      SSH: {sshUser}@{sshHost}
                    </span>
                    <br />
                    <span className="font-mono">
                      Database host: 127.0.0.1, port {ext?.host_port}
                    </span>
                    <br />
                    <span>
                      the host and port are how the database looks{" "}
                      <em>from the server</em>, not from your machine
                    </span>
                  </dd>
                </div>
              </dl>
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

const MB = 1024 * 1024;

/**
 * Advanced settings: editable CPU/memory limits applied live, plus read-only
 * image and managed-volume info. Image upgrades are intentionally not editable
 * here — a major version bump is a separate guarded flow.
 */
function AdvancedCard({ db }: { db: Database }) {
  const update = useSetDatabaseResources(db.project_id, db.id);
  const upgrade = useUpgradeDatabase(db.project_id, db.id);
  const { data: volume, isLoading: volumeLoading } = useDatabaseVolume(
    db.project_id,
    db.id,
  );
  const resourcesForm = useForm({
    defaultValues: {
      cpu: String(db.cpu_limit ?? 0),
      memMb: String(db.memory_limit ? Math.round(db.memory_limit / MB) : 0),
    },
    onSubmit: ({ value }) => {
      toast.promise(
        update.mutateAsync({
          cpu_limit: Number(value.cpu),
          memory_limit: Number(value.memMb) * MB,
        }),
        {
          loading: "Applying resource limits…",
          success: "Resource limits updated",
          error: (err) => err.message,
        },
      );
    },
  });

  const upgradeForm = useForm({
    defaultValues: { version: "" },
    onSubmit: ({ value }) => {
      toast.promise(upgrade.mutateAsync(value.version.trim()), {
        loading: "Starting upgrade…",
        success: "Upgrade started — the database will be briefly offline",
        error: (err) => err.message,
      });
      upgradeForm.reset();
    },
  });

  const nonNegative = z
    .string()
    .refine((v) => v === "" || Number(v) >= 0, "Cannot be negative");

  return (
    <Card>
      <CardHeader>
        <CardTitle>Advanced</CardTitle>
        <CardDescription>
          Resource limits apply live. 0 means unlimited.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-6">
        {/* Resource limits — editable */}
        <form
          onSubmit={(e) => {
            e.preventDefault();
            e.stopPropagation();
            resourcesForm.handleSubmit();
          }}
          className="space-y-6"
        >
          <div className="grid gap-4 sm:grid-cols-2">
            <resourcesForm.Field
              name="cpu"
              validators={{ onChange: nonNegative }}
              children={(field) => {
                const error = fieldError(field.state.meta.errors);
                return (
                  <Field data-invalid={!!error}>
                    <FieldLabel htmlFor="db-cpu">CPU limit (cores)</FieldLabel>
                    <Input
                      id="db-cpu"
                      type="number"
                      min={0}
                      step={0.1}
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                      aria-invalid={!!error}
                    />
                    {error && <FieldError>{error}</FieldError>}
                  </Field>
                );
              }}
            />
            <resourcesForm.Field
              name="memMb"
              validators={{ onChange: nonNegative }}
              children={(field) => {
                const error = fieldError(field.state.meta.errors);
                return (
                  <Field data-invalid={!!error}>
                    <FieldLabel htmlFor="db-mem">Memory limit (MB)</FieldLabel>
                    <Input
                      id="db-mem"
                      type="number"
                      min={0}
                      step={64}
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                      aria-invalid={!!error}
                    />
                    {error && <FieldError>{error}</FieldError>}
                  </Field>
                );
              }}
            />
          </div>
          <Button type="submit" size="sm" disabled={update.isPending}>
            {update.isPending ? "Saving…" : "Save resource limits"}
          </Button>
        </form>

        <Separator />

        {/* Image — read-only, with guarded upgrade for known engines */}
        <div className="flex items-start justify-between gap-3">
          <div>
            <p className="text-sm font-medium">Image</p>
            <p className="text-text-faint text-xs">
              {db.type === "other"
                ? "Change it by recreating the database."
                : "Major version upgrades dump, rebuild, and restore (brief downtime)."}
            </p>
          </div>
          <div className="flex items-center gap-2">
            <Badge variant="outline" className="font-mono">
              {imageLabel(db)}
            </Badge>
            {dbUpgradable(db) && (
              <AlertDialog>
                <AlertDialogTrigger
                  render={<Button variant="outline" size="sm" />}
                >
                  Upgrade
                </AlertDialogTrigger>
                <AlertDialogContent>
                  <form
                    onSubmit={(e) => {
                      e.preventDefault();
                      e.stopPropagation();
                      upgradeForm.handleSubmit();
                    }}
                    className="space-y-4"
                  >
                    <AlertDialogHeader>
                      <AlertDialogTitle>
                        Upgrade {db.type} version
                      </AlertDialogTitle>
                      <AlertDialogDescription>
                        This takes a dump of{" "}
                        <span className="font-medium">{db.name}</span>, rebuilds
                        the container at the new version, and restores the data.
                        The database is briefly offline. If anything fails it
                        rolls back to {db.version}. A pre-upgrade backup is
                        kept.
                      </AlertDialogDescription>
                    </AlertDialogHeader>
                    <upgradeForm.Field
                      name="version"
                      children={(field) => (
                        <div className="space-y-2">
                          <Label htmlFor="upgrade-version">
                            Target version (current: {db.version})
                          </Label>
                          <Input
                            id="upgrade-version"
                            value={field.state.value}
                            onBlur={field.handleBlur}
                            onChange={(e) => field.handleChange(e.target.value)}
                            placeholder="e.g. 17"
                          />
                          <p className="text-text-faint text-xs">
                            Enter the same version to refresh it to the latest
                            patch.
                          </p>
                        </div>
                      )}
                    />
                    <AlertDialogFooter>
                      <AlertDialogCancel type="button">
                        Cancel
                      </AlertDialogCancel>
                      <upgradeForm.Subscribe
                        selector={(s) => s.values.version}
                        children={(version) => (
                          <AlertDialogAction
                            type="submit"
                            disabled={!version.trim()}
                          >
                            Upgrade
                          </AlertDialogAction>
                        )}
                      />
                    </AlertDialogFooter>
                  </form>
                </AlertDialogContent>
              </AlertDialog>
            )}
          </div>
        </div>

        {/* Data directory — read-only ("other" only) */}
        {db.type === "other" && db.data_dir && (
          <div className="flex items-start justify-between gap-3">
            <div>
              <p className="text-sm font-medium">Data directory</p>
              <p className="text-text-faint text-xs">
                Set at creation; cannot be changed.
              </p>
            </div>
            <Badge variant="outline" className="font-mono">
              {db.data_dir}
            </Badge>
          </div>
        )}

        {/* Volume — read-only */}
        <div className="flex items-start justify-between gap-3">
          <div>
            <p className="text-sm font-medium">Volume</p>
            <p className="text-text-faint text-xs">Managed automatically.</p>
          </div>
          <div className="text-right">
            {volume ? (
              <>
                <p className="font-mono text-xs">{volume.name}</p>
                <p className="text-text-faint flex items-center justify-end gap-1 text-xs">
                  {volume.size_bytes != null ? (
                    formatBytes(volume.size_bytes)
                  ) : (
                    <span title="The size scan timed out on a busy host.">
                      size unavailable
                    </span>
                  )}
                </p>
              </>
            ) : volumeLoading ? (
              <p className="text-text-faint flex items-center justify-end gap-1 text-xs">
                <Loader2 className="size-3 animate-spin" aria-hidden="true" />
                Calculating…
              </p>
            ) : (
              <p className="text-text-faint text-xs">—</p>
            )}
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

function BackupsTab({ db, canDelete }: { db: Database; canDelete: boolean }) {
  const { data: configs, isLoading } = useDatabaseBackupConfigs(
    db.project_id,
    db.id,
  );
  const { data: destinations } = useBackupDestinations(db.project_id);
  const { data: backups } = useDatabaseBackups(db.project_id, db.id);
  const backup = useBackupDatabase(db.project_id, db.id);
  const runConfig = useRunBackupConfig(db.project_id, db.id);
  const deleteConfig = useDeleteBackupConfig(db.project_id, db.id);

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<DatabaseBackupConfig | null>(null);
  const [sheetOpen, setSheetOpen] = useState(false);
  const [selected, setSelected] = useState<DatabaseBackupConfig | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<DatabaseBackupConfig | null>(
    null,
  );

  const destName = (id: string) =>
    destinations?.find((d) => d.id === id)?.name ?? "Unknown destination";
  const noDestinations = destinations?.length === 0;
  const anyRunning = backups?.some((b) => b.status === "running") ?? false;

  const openAdd = () => {
    setEditing(null);
    setFormOpen(true);
  };
  const openManage = (cfg: DatabaseBackupConfig) => {
    setSelected(cfg);
    setSheetOpen(true);
  };
  const openEdit = (cfg: DatabaseBackupConfig) => {
    setSheetOpen(false);
    setEditing(cfg);
    setFormOpen(true);
  };

  const runNow = (cfg: DatabaseBackupConfig) => {
    toast.promise(runConfig.mutateAsync(cfg.id), {
      loading: "Starting backup…",
      success: "Backup started",
      error: (err) => err.message,
    });
  };

  const handleDeleteConfig = () => {
    if (!deleteTarget) return;
    toast.promise(deleteConfig.mutateAsync(deleteTarget.id), {
      loading: "Deleting backup configuration…",
      success: () => {
        setDeleteTarget(null);
        return "Backup configuration deleted";
      },
      error: (err) => err.message,
    });
  };

  const handleAdHocBackup = () => {
    toast.promise(backup.mutateAsync(), {
      loading: "Starting backup…",
      success: "Backup started",
      error: (err) => err.message,
    });
  };

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <CardTitle>Backups</CardTitle>
            <CardDescription>
              Schedule recurring backups to a project destination, or run an
              ad-hoc backup now.
            </CardDescription>
          </div>
          <div className="flex flex-wrap gap-2">
            <Button
              size="sm"
              variant="outline"
              onClick={handleAdHocBackup}
              disabled={backup.isPending || anyRunning}
            >
              <DatabaseBackupIcon className="mr-1 h-4 w-4" />
              Back up now
            </Button>
            <Button size="sm" onClick={openAdd} disabled={noDestinations}>
              <Plus className="mr-1 h-4 w-4" />
              Add Backup
            </Button>
          </div>
        </div>
      </CardHeader>
      <CardContent>
        {noDestinations && (
          <p className="text-text-faint mb-3 text-sm">
            Add a destination on the project{" "}
            <span className="font-medium">Backups</span> tab before scheduling
            backups.
          </p>
        )}
        {isLoading ? (
          <Skeleton className="h-16 w-full" />
        ) : !configs || configs.length === 0 ? (
          <p className="text-text-faint py-4 text-center text-sm">
            No scheduled backups yet.
          </p>
        ) : (
          <ul className="divide-border divide-y">
            {configs.map((c) => (
              <li
                key={c.id}
                className="flex items-center justify-between gap-3 py-3"
              >
                <div className="min-w-0 space-y-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="font-medium">
                      {destName(c.destination_id)}
                    </span>
                    {c.enabled ? (
                      <Badge variant="outline">Active</Badge>
                    ) : (
                      <Badge variant="secondary">Disabled</Badge>
                    )}
                  </div>
                  <p className="text-text-faint text-xs">
                    <code className="font-mono">{c.schedule}</code>
                    {c.all_databases
                      ? " · all databases"
                      : ` · ${c.databases.join(", ")}`}
                    {c.prefix ? ` · ${c.prefix}` : ""}
                    {c.keep_latest != null ? ` · keep ${c.keep_latest}` : ""}
                  </p>
                </div>
                <div className="flex shrink-0 items-center gap-1">
                  <IconAction
                    label="Back up now"
                    size="icon-sm"
                    disabled={runConfig.isPending || anyRunning}
                    onClick={() => runNow(c)}
                  >
                    <DatabaseBackupIcon className="h-4 w-4" />
                  </IconAction>
                  <IconAction
                    label="Manage Histories"
                    size="icon-sm"
                    onClick={() => openManage(c)}
                  >
                    <History className="h-4 w-4" />
                  </IconAction>
                  <IconAction
                    label="Edit"
                    size="icon-sm"
                    onClick={() => openEdit(c)}
                  >
                    <Pencil className="h-4 w-4" />
                  </IconAction>
                  <IconAction
                    label="Delete"
                    size="icon-sm"
                    destructive
                    onClick={() => setDeleteTarget(c)}
                  >
                    <Trash2 className="h-4 w-4" />
                  </IconAction>
                </div>
              </li>
            ))}
          </ul>
        )}
      </CardContent>

      <BackupConfigFormDialog
        projectId={db.project_id}
        databaseId={db.id}
        config={editing}
        open={formOpen}
        onOpenChange={setFormOpen}
      />
      <BackupConfigRunsSheet
        db={db}
        config={selected}
        destinationName={
          selected ? destName(selected.destination_id) : undefined
        }
        open={sheetOpen}
        onOpenChange={setSheetOpen}
        canDelete={canDelete}
      />

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(o) => !o && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete backup configuration?</AlertDialogTitle>
            <AlertDialogDescription>
              This removes the schedule and deletes the backups it produced
              (local files and remote objects). This can't be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDeleteConfig}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}
