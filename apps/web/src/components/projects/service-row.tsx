import { useMemo, useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { DataTable } from "@/components/ui/data-table";
import {
  AppWindowIcon,
  ArrowUpRightIcon,
  DatabaseIcon,
  Loader2Icon,
  MoreHorizontalIcon,
  PlayIcon,
  RocketIcon,
  RotateCcwIcon,
  ScrollTextIcon,
  SquareIcon,
  Trash2Icon,
} from "lucide-react";
import { toast } from "sonner";
import type {
  Application,
  Database,
  ProjectMetrics,
  ServiceMetrics,
} from "@/lib/types";
import { formatCpuCell, formatMemoryCell } from "./usage-format";
import {
  useDeleteApplication,
  useDeployApplication,
  useRestartApplication,
  useStartApplication,
  useStopApplication,
} from "@/lib/hooks/use-applications";
import {
  useDatabaseDeletionImpact,
  useDeleteDatabase,
  useRestartDatabase,
  useStartDatabase,
  useStopDatabase,
} from "@/lib/hooks/use-databases";
import { queryKeys } from "@/lib/hooks/query-keys";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { StatusPill } from "@/components/ui/status-pill";
import { PendingChangeBadge } from "@/lib/components/pending-change-badge";
import { DatabaseReloadBadge } from "@/lib/components/database-reload-badge";
import { formatUptime, formatList } from "@/lib/utils/format";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Tooltip,
  TooltipContent,
  TooltipPositioner,
  TooltipTrigger,
} from "@/components/ui/tooltip";

export type ServiceRowItem =
  | { kind: "application"; data: Application }
  | { kind: "database"; data: Database };

// Icon-only row action with a hover/focus tooltip label.
function IconAction({
  label,
  onClick,
  children,
  className,
}: {
  label: string;
  onClick: () => void;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            variant="ghost"
            size="icon"
            aria-label={label}
            onClick={onClick}
            className={className}
          />
        }
      >
        {children}
      </TooltipTrigger>
      <TooltipPositioner>
        <TooltipContent>{label}</TooltipContent>
      </TooltipPositioner>
    </Tooltip>
  );
}

const TRANSIENT = new Set([
  "building",
  "deploying",
  "pending",
  "queued",
  "creating",
  "restarting",
  "provisioning",
  "upgrading",
  "backing_up",
]);

const isTransient = (status: string) => TRANSIENT.has(status.toLowerCase());

const RUNNING = new Set(["running", "ready"]);

function ApplicationActions({
  projectId,
  app,
  canDelete,
}: {
  projectId: string;
  app: Application;
  canDelete: boolean;
}) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const deploy = useDeployApplication(projectId, app.id);
  const stop = useStopApplication(projectId, app.id);
  const start = useStartApplication(projectId, app.id);
  const restart = useRestartApplication(projectId, app.id);
  const del = useDeleteApplication(projectId);
  const [confirmOpen, setConfirmOpen] = useState(false);

  // Lifecycle hooks invalidate the app *detail* query; nudge the list too so the
  // row reflects the new status without waiting for the next poll.
  const refreshList = () =>
    qc.invalidateQueries({ queryKey: queryKeys.applications.all(projectId) });
  const onSuccess = { onSuccess: refreshList };

  const status = app.status.toLowerCase();
  const busy =
    isTransient(status) ||
    stop.isPending ||
    start.isPending ||
    restart.isPending;

  let primary: React.ReactNode = null;
  if (busy) {
    primary = (
      <Button variant="ghost" size="icon" disabled aria-label="Working">
        <Loader2Icon aria-hidden="true" className="size-4 animate-spin" />
      </Button>
    );
  } else if (status === "running") {
    primary = (
      <>
        <IconAction
          label="Stop"
          onClick={() => stop.mutate()}
          className="text-destructive hover:bg-destructive/10 hover:text-destructive"
        >
          <SquareIcon aria-hidden="true" className="size-4" />
        </IconAction>
        <IconAction label="Restart" onClick={() => restart.mutate()}>
          <RotateCcwIcon aria-hidden="true" className="size-4" />
        </IconAction>
      </>
    );
  } else if (
    status === "failed" ||
    status === "error" ||
    status === "crashed"
  ) {
    primary = (
      <IconAction label="Restart" onClick={() => restart.mutate()}>
        <RotateCcwIcon aria-hidden="true" className="size-4" />
      </IconAction>
    );
  } else {
    primary = (
      <IconAction label="Start" onClick={() => start.mutate()}>
        <PlayIcon aria-hidden="true" className="size-4" />
      </IconAction>
    );
  }

  const open = () =>
    navigate({
      to: "/projects/$projectId/applications/$applicationId",
      params: { projectId, applicationId: app.id },
    });

  return (
    <div className="flex items-center justify-end gap-1">
      {primary}
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <Button
              variant="ghost"
              size="icon"
              aria-label="More actions"
              title="More"
            />
          }
        >
          <MoreHorizontalIcon aria-hidden="true" className="size-4" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onClick={open}>
            <ArrowUpRightIcon aria-hidden="true" />
            Open
          </DropdownMenuItem>
          <DropdownMenuItem
            onClick={() =>
              navigate({
                to: "/projects/$projectId/applications/$applicationId/logs",
                params: { projectId, applicationId: app.id },
              })
            }
          >
            <ScrollTextIcon aria-hidden="true" />
            View logs
          </DropdownMenuItem>
          <DropdownMenuItem
            disabled={busy}
            onClick={() => deploy.mutate(undefined, onSuccess)}
          >
            <RocketIcon aria-hidden="true" />
            Redeploy
          </DropdownMenuItem>
          {canDelete && (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                variant="destructive"
                onClick={() => setConfirmOpen(true)}
              >
                <Trash2Icon aria-hidden="true" />
                Delete
              </DropdownMenuItem>
            </>
          )}
        </DropdownMenuContent>
      </DropdownMenu>

      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete {app.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              This permanently deletes the application and stops its container.
              This action cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() =>
                toast.promise(del.mutateAsync(app.id), {
                  loading: "Deleting application…",
                  success: `${app.name} deleted`,
                  error: (err) => err.message,
                })
              }
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function DatabaseActions({
  projectId,
  db,
  canDelete,
}: {
  projectId: string;
  db: Database;
  canDelete: boolean;
}) {
  const navigate = useNavigate();
  const stop = useStopDatabase(projectId, db.id);
  const start = useStartDatabase(projectId, db.id);
  const restart = useRestartDatabase(projectId, db.id);
  const del = useDeleteDatabase(projectId);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [deleteConfirm, setDeleteConfirm] = useState("");
  // Unchecked by default, like the detail page: deleting the database is
  // recoverable from a backup, deleting the backups with it is not.
  const [deleteBackups, setDeleteBackups] = useState(false);
  const {
    data: deleteImpact,
    isSuccess: impactKnown,
    isError: impactFailed,
  } = useDatabaseDeletionImpact(projectId, db.id, confirmOpen);

  // Backups now survive the database, so this gate is no longer about them
  // being destroyed — it is about a quick action from a list row being a
  // heavier decision than it looks when there is history attached. Left
  // deliberately unchanged: correcting the wording below should not also
  // quietly weaken a confirmation.
  //
  // This gate fails CLOSED. "No backups" is a claim that needs an answer from
  // the server, so until one arrives the button stays disabled, and if the
  // query errors we assume backups exist and demand the typed name. Treating
  // an unanswered query as "nothing to lose" would put the destructive path
  // one click away in exactly the moments the server is unwell — and React
  // Query stops retrying, so that state persists for as long as the dialog is
  // open rather than resolving on its own.
  const backupsAtRisk = impactFailed || (deleteImpact?.backup_count ?? 0) > 0;
  const confirmSatisfied = backupsAtRisk
    ? deleteConfirm.trim() === db.name.trim()
    : impactKnown;

  const status = db.status.toLowerCase();
  const busy =
    isTransient(status) ||
    stop.isPending ||
    start.isPending ||
    restart.isPending;

  let primary: React.ReactNode = null;
  if (busy) {
    primary = (
      <Button variant="ghost" size="icon" disabled aria-label="Working">
        <Loader2Icon aria-hidden="true" className="size-4 animate-spin" />
      </Button>
    );
  } else if (status === "running") {
    primary = (
      <>
        <IconAction
          label="Stop"
          onClick={() => stop.mutate()}
          className="text-destructive hover:bg-destructive/10 hover:text-destructive"
        >
          <SquareIcon aria-hidden="true" className="size-4" />
        </IconAction>
        <IconAction label="Restart" onClick={() => restart.mutate()}>
          <RotateCcwIcon aria-hidden="true" className="size-4" />
        </IconAction>
      </>
    );
  } else if (status === "failed" || status === "error") {
    primary = (
      <IconAction label="Restart" onClick={() => restart.mutate()}>
        <RotateCcwIcon aria-hidden="true" className="size-4" />
      </IconAction>
    );
  } else {
    primary = (
      <IconAction label="Start" onClick={() => start.mutate()}>
        <PlayIcon aria-hidden="true" className="size-4" />
      </IconAction>
    );
  }

  const open = () =>
    navigate({
      to: "/projects/$projectId/databases/$databaseId",
      params: { projectId, databaseId: db.id },
    });

  return (
    <div className="flex items-center justify-end gap-1">
      {primary}
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <Button
              variant="ghost"
              size="icon"
              aria-label="More actions"
              title="More"
            />
          }
        >
          <MoreHorizontalIcon aria-hidden="true" className="size-4" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onClick={open}>
            <ArrowUpRightIcon aria-hidden="true" />
            Open
          </DropdownMenuItem>
          {canDelete && (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                variant="destructive"
                onClick={() => setConfirmOpen(true)}
              >
                <Trash2Icon aria-hidden="true" />
                Delete
              </DropdownMenuItem>
            </>
          )}
        </DropdownMenuContent>
      </DropdownMenu>

      <AlertDialog
        open={confirmOpen}
        onOpenChange={(o) => {
          setConfirmOpen(o);
          if (o) {
            setDeleteConfirm("");
            setDeleteBackups(false);
          }
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete {db.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              This permanently deletes the database and its volume. This action
              cannot be undone.
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
                , and stay listed under the project&apos;s Backups tab. You can
                restore a replacement database from them.
              </AlertDialogDescription>
            ) : null}
            {impactFailed ? (
              <AlertDialogDescription className="text-destructive font-medium">
                Could not check what this database has. Its backups are kept
                either way, but confirm by name to continue.
              </AlertDialogDescription>
            ) : null}
          </AlertDialogHeader>
          {deleteImpact && deleteImpact.backup_count > 0 ? (
            <div className="flex items-start gap-2.5">
              <Checkbox
                id={`delete-db-backups-${db.id}`}
                checked={deleteBackups}
                onCheckedChange={(checked) =>
                  setDeleteBackups(checked === true)
                }
                className="mt-0.5"
              />
              <Label
                htmlFor={`delete-db-backups-${db.id}`}
                className="leading-snug font-normal"
              >
                Also delete{" "}
                {deleteImpact.backup_count === 1
                  ? "this backup"
                  : "these backups"}
                <span className="text-text-muted block text-xs">
                  {deleteImpact.backup_destinations.length > 0
                    ? "Erases the archives, including the remote copies. This cannot be undone."
                    : "Erases the archives. This cannot be undone."}
                </span>
              </Label>
            </div>
          ) : null}
          {backupsAtRisk ? (
            <div className="space-y-2">
              <Label
                htmlFor={`delete-db-confirm-${db.id}`}
                className="font-normal"
              >
                Type{" "}
                <span className="text-foreground font-medium">{db.name}</span>{" "}
                to confirm.
              </Label>
              <Input
                id={`delete-db-confirm-${db.id}`}
                value={deleteConfirm}
                onChange={(e) => setDeleteConfirm(e.target.value)}
                autoComplete="off"
                autoCorrect="off"
                spellCheck={false}
              />
            </div>
          ) : null}
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={!confirmSatisfied}
              onClick={() =>
                toast.promise(
                  del.mutateAsync({ databaseId: db.id, deleteBackups }),
                  {
                    loading: "Deleting database…",
                    success: `${db.name} deleted`,
                    error: (err) => err.message,
                  },
                )
              }
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

/** Inline lifecycle controls (start/stop/restart + kebab menu) for a service. */
export function ServiceActions({
  projectId,
  item,
  canDelete,
}: {
  projectId: string;
  item: ServiceRowItem;
  canDelete: boolean;
}) {
  return item.kind === "application" ? (
    <ApplicationActions
      projectId={projectId}
      app={item.data}
      canDelete={canDelete}
    />
  ) : (
    <DatabaseActions
      projectId={projectId}
      db={item.data}
      canDelete={canDelete}
    />
  );
}

function NameCell({
  projectId,
  item,
}: {
  projectId: string;
  item: ServiceRowItem;
}) {
  const isApp = item.kind === "application";
  return (
    <div className="flex min-w-0 items-center gap-3">
      <div className="bg-elev text-text-muted grid size-9 shrink-0 place-items-center rounded-lg">
        {isApp ? (
          <AppWindowIcon aria-hidden="true" className="size-4.5" />
        ) : (
          <DatabaseIcon aria-hidden="true" className="size-4.5" />
        )}
      </div>
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          {isApp ? (
            <Link
              to="/projects/$projectId/applications/$applicationId"
              params={{ projectId, applicationId: item.data.id }}
              className="hover:text-brand-text truncate text-sm font-medium transition-colors"
            >
              {item.data.name}
            </Link>
          ) : (
            <Link
              to="/projects/$projectId/databases/$databaseId"
              params={{ projectId, databaseId: item.data.id }}
              className="hover:text-brand-text truncate text-sm font-medium transition-colors"
            >
              {item.data.name}
            </Link>
          )}
          <span className="text-text-faint hidden truncate font-mono text-xs lg:inline">
            {item.data.slug}
          </span>
        </div>
        <Badge variant="outline" className="mt-1 capitalize">
          {item.data.type}
        </Badge>
      </div>
    </div>
  );
}

function StatusCell({
  item,
  metrics,
}: {
  item: ServiceRowItem;
  metrics?: ServiceMetrics;
}) {
  const running = RUNNING.has(item.data.status.toLowerCase());
  return (
    <div className="flex flex-col items-start gap-1">
      {/* The pill says what the container is doing; the second badge says the
          saved state has drifted from it. They are different facts, so they sit
          side by side rather than one replacing the other. Deliberately not
          flex-wrap: the column is sized to min-content (w-px), and a wrapping
          row's min-content is its widest child, which would stack the two.
          Applications flag config/source drift; databases flag a container that
          was deleted (Reload recreates it). */}
      <div className="flex items-center gap-1.5">
        <StatusPill status={item.data.status} />
        {item.kind === "application" ? (
          <PendingChangeBadge app={item.data} pulse={false} />
        ) : (
          <DatabaseReloadBadge db={item.data} pulse={false} />
        )}
      </div>
      {running && metrics?.uptime_seconds ? (
        <span className="text-text-faint text-xs">
          Up {formatUptime(metrics.uptime_seconds)}
        </span>
      ) : null}
    </div>
  );
}

function EndpointCell({
  item,
  metrics,
}: {
  item: ServiceRowItem;
  metrics?: ServiceMetrics;
}) {
  const port =
    item.kind === "database"
      ? (item.data.internal_port ?? undefined)
      : undefined;
  const effectivePort = port ?? metrics?.port;
  const domain = metrics?.domain;
  return (
    <div className="text-text-faint min-w-0 font-mono text-xs">
      {effectivePort ? <span>:{effectivePort}</span> : null}
      {domain ? <div className="text-text-muted truncate">{domain}</div> : null}
      {!effectivePort && !domain ? "—" : null}
    </div>
  );
}

function usageText(item: ServiceRowItem, metrics: ServiceMetrics | undefined) {
  return metrics && RUNNING.has(item.data.status.toLowerCase())
    ? metrics
    : null;
}

/**
 * Standard column definitions for the project services table. `metricsFor`
 * resolves live metrics per row (applications only); databases have none.
 */
function buildColumns(
  projectId: string,
  canDelete: boolean,
  metricsFor: (item: ServiceRowItem) => ServiceMetrics | undefined,
): ColumnDef<ServiceRowItem>[] {
  return [
    {
      id: "name",
      header: "Name / Type",
      cell: ({ row }) => <NameCell projectId={projectId} item={row.original} />,
    },
    {
      id: "status",
      header: "Status",
      // w-px collapses the column to its min-content width in an auto-layout
      // table, so Status takes only the room its pills need instead of an equal
      // share; the freed space goes to the name/endpoint columns.
      meta: { className: "w-px", headerClassName: "w-px" },
      cell: ({ row }) => (
        <StatusCell item={row.original} metrics={metricsFor(row.original)} />
      ),
    },
    {
      id: "endpoint",
      header: "Port · Domain",
      meta: {
        className: "hidden md:table-cell",
        headerClassName: "hidden md:table-cell",
      },
      cell: ({ row }) => (
        <EndpointCell item={row.original} metrics={metricsFor(row.original)} />
      ),
    },
    {
      id: "cpu",
      header: "CPU",
      meta: {
        className: "text-text-muted hidden font-mono text-xs md:table-cell",
        headerClassName: "hidden md:table-cell",
      },
      cell: ({ row }) => {
        return formatCpuCell(usageText(row.original, metricsFor(row.original)));
      },
    },
    {
      id: "memory",
      header: "Memory",
      meta: {
        className: "text-text-muted hidden font-mono text-xs md:table-cell",
        headerClassName: "hidden md:table-cell",
      },
      cell: ({ row }) => {
        return formatMemoryCell(
          usageText(row.original, metricsFor(row.original)),
        );
      },
    },
    {
      id: "actions",
      header: "",
      meta: { headerClassName: "text-right", className: "text-right" },
      cell: ({ row }) => (
        <ServiceActions
          projectId={projectId}
          item={row.original}
          canDelete={canDelete}
        />
      ),
    },
  ];
}

/** Standard column table of a project's services (applications + databases). */
export function ServicesTable({
  projectId,
  items,
  metrics,
  isLoading,
  canDelete,
}: {
  projectId: string;
  items: ServiceRowItem[];
  metrics?: ProjectMetrics;
  isLoading?: boolean;
  canDelete: boolean;
}) {
  const columns = useMemo(
    () =>
      buildColumns(projectId, canDelete, (item) =>
        item.kind === "application" ? metrics?.[item.data.id] : undefined,
      ),
    [projectId, canDelete, metrics],
  );

  return (
    <DataTable
      columns={columns}
      data={items}
      getRowId={(it) => it.data.id}
      isLoading={isLoading}
      emptyMessage="No services match the current filters."
    />
  );
}
