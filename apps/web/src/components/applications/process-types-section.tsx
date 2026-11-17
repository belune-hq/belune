import { useState } from "react";
import { toast } from "sonner";
import {
  BoxesIcon,
  ChevronDownIcon,
  InfoIcon,
  MoreHorizontalIcon,
  PencilIcon,
  RefreshCwIcon,
  RotateCwIcon,
  PlusIcon,
  ScrollTextIcon,
  SearchIcon,
  Trash2Icon,
  TriangleAlertIcon,
} from "lucide-react";
import { Link } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
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
import { StatusPill } from "@/components/ui/status-pill";
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
  useDeleteProcessType,
  useProcessTypes,
  useReloadInstance,
  useRestartInstance,
} from "@/lib/hooks/use-process-types";
import { shortContainerName } from "@/lib/utils/container-name";
import { useVolumes } from "@/lib/hooks/use-volumes";
import { useAuthStore } from "@/lib/stores/auth";
import type { ProcessInstance, ProcessType } from "@/lib/types";
import { ProcessTypeForm } from "./process-type-form";

interface Props {
  projectId: string;
  applicationId: string;
}

export function ProcessTypesSection({ projectId, applicationId }: Props) {
  const { data: types, isLoading } = useProcessTypes(projectId, applicationId);
  const del = useDeleteProcessType(projectId, applicationId);
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<ProcessType | null>(null);
  const [removeTarget, setRemoveTarget] = useState<ProcessType | null>(null);

  const routable = types?.find((t) => t.routable);
  const workers = (types ?? []).filter((t) => !t.routable);
  const openEdit = (pt: ProcessType) => {
    setEditing(pt);
    setFormOpen(true);
  };

  const submitRemove = () => {
    if (!removeTarget) return;
    toast.promise(
      del.mutateAsync(removeTarget.id).then((res) => {
        setRemoveTarget(null);
        if (res.warning) toast.warning(res.warning);
      }),
      {
        loading: "Removing process type…",
        success: "Process type removed",
        error: (err) => err.message,
      },
    );
  };

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <CardTitle className="flex items-center gap-2">
              <BoxesIcon aria-hidden="true" className="size-4" />
              Processes
            </CardTitle>
            <CardDescription className="mt-1">
              Run a different command from the same built image — a queue worker
              or a scheduler beside the web process. Each type gets its own
              containers. Only the routable type receives traffic.
            </CardDescription>
          </div>
          <Button
            size="sm"
            className="shrink-0"
            onClick={() => {
              setEditing(null);
              setFormOpen(true);
            }}
          >
            <PlusIcon aria-hidden="true" className="size-4" />
            Add Process Type
          </Button>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="bg-muted/40 text-muted-foreground flex items-start gap-2 rounded-lg border p-3 text-sm">
          <InfoIcon aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
          <span>
            Adding or editing a type takes effect on the{" "}
            <strong>next deploy</strong>, because a container needs a built
            image to start from. Removing one stops and removes its container{" "}
            <strong>immediately</strong>. Volumes and file mounts are mounted
            into every type.
          </span>
        </div>
        {/* One image can run several HTTP processes (a compiled backend and a frontend,
            say), but exactly one type is routable and domains resolve through it, so a
            second web process is reachable only inside the project network. Without
            this, an operator adds a type, gives it a port and waits for traffic that
            never comes. */}
        <p className="text-muted-foreground text-sm">
          A second HTTP process works, but it can never receive a domain: only
          the routable type does. It is reachable only from inside the project
          network. For two public hostnames, use two applications.
        </p>

        {isLoading ? (
          <div className="space-y-3">
            {[1, 2].map((i) => (
              <Skeleton key={i} className="h-20 w-full" />
            ))}
          </div>
        ) : !types || types.length === 0 ? (
          <p className="text-muted-foreground py-4 text-center text-sm">
            No process types.
          </p>
        ) : (
          <div className="space-y-6">
            {routable && (
              <section aria-labelledby="proc-routable" className="space-y-3">
                <h3 id="proc-routable" className="text-sm font-medium">
                  Routable
                </h3>
                <ul className="grid gap-3 md:grid-cols-2">
                  <TypeCard
                    pt={routable}
                    projectId={projectId}
                    applicationId={applicationId}
                    onEdit={() => openEdit(routable)}
                    onRemove={() => setRemoveTarget(routable)}
                  />
                </ul>
              </section>
            )}
            <section aria-labelledby="proc-workers" className="space-y-3">
              <h3 id="proc-workers" className="text-sm font-medium">
                Workers
              </h3>
              {workers.length === 0 ? (
                <p className="text-muted-foreground text-sm">
                  No workers. A worker runs a different command from the same
                  image, such as a queue consumer or a scheduler.
                </p>
              ) : (
                <ul className="grid gap-3 md:grid-cols-2">
                  {workers.map((pt) => (
                    <TypeCard
                      key={pt.id}
                      pt={pt}
                      projectId={projectId}
                      applicationId={applicationId}
                      onEdit={() => openEdit(pt)}
                      onRemove={() => setRemoveTarget(pt)}
                    />
                  ))}
                </ul>
              )}
            </section>
          </div>
        )}
      </CardContent>

      <ProcessTypeForm
        projectId={projectId}
        applicationId={applicationId}
        processType={editing}
        open={formOpen}
        onOpenChange={setFormOpen}
      />

      <AlertDialog
        open={removeTarget !== null}
        onOpenChange={(open) => {
          if (!open) setRemoveTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Remove {removeTarget?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              Its container is stopped and removed right away, not on the next
              deploy. It gets a few seconds to finish what it is doing, then it
              is killed, so anything still running stops with it.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={submitRemove} disabled={del.isPending}>
              Remove process type
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}

const HEALTH_KIND_LABEL = {
  http: "HTTP probe",
  command: "Command",
  none: "None",
} as const;

function healthText(pt: ProcessType): string {
  const h = pt.resolved_health;
  const label = HEALTH_KIND_LABEL[h.kind];
  if (h.kind === "command" && pt.health_check_command) {
    return `${label}: ${pt.health_check_command}`;
  }
  // Said in the operator's words, and before the source suffix: someone reading this
  // must not conclude a deploy is verified when nothing is checked.
  if (h.inert) return `${label} — no path set, so nothing is checked`;
  if (h.source === "inherited") return `${label} (the application's)`;
  if (h.source === "default") return `${label} (default)`;
  return label;
}

// The short word on the card. "Default" for a check the type did not choose; the kind
// when it did. The exact detail (path, command, inherited or own) is in the tooltip.
function healthLabel(pt: ProcessType): string {
  const h = pt.resolved_health;
  if (h.source !== "set") return "Default";
  return { http: "HTTP probe", command: "Command", none: "None" }[h.kind];
}

// More rows than this collapse behind "N more". Expanded, the list scrolls inside the
// card past EXPANDED_MAX_ROWS rather than stretching it: MAX_REPLICAS is 20 and twenty
// rows would push the next card off the screen.
const COLLAPSED_ROWS = 3;
const EXPANDED_MAX_ROWS = 8;

function TypeCard({
  pt,
  projectId,
  applicationId,
  onEdit,
  onRemove,
}: {
  pt: ProcessType;
  projectId: string;
  applicationId: string;
  onEdit: () => void;
  onRemove: () => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const hidden = Math.max(pt.instances.length - COLLAPSED_ROWS, 0);
  const shown = expanded ? pt.instances : pt.instances.slice(0, COLLAPSED_ROWS);

  return (
    <li className="space-y-3 rounded-lg border p-4" data-testid="type-card">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          {/* The real name, never a generic "Main": container names, the Logs filter,
              the Terminal selector, scheduled-run history and the per-line log label
              all address a type by it. */}
          <span className="truncate font-mono font-medium">{pt.name}</span>
          {pt.routable && (
            <span className="border-primary/20 bg-primary/10 text-brand-text rounded-full border px-2 py-0.5 text-xs font-medium">
              Routable
            </span>
          )}
        </div>
        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <Button
                size="icon-sm"
                variant="ghost"
                aria-label={`Actions for ${pt.name}`}
              />
            }
          >
            <MoreHorizontalIcon aria-hidden="true" className="size-4" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onClick={onEdit}>
              <PencilIcon aria-hidden="true" />
              Edit
            </DropdownMenuItem>
            {/* No Stop, deliberately: nothing enforces replicas between deploys, so a
                stopped container would stay down while the card reads "Replicas 3".
                Replicas 0 is the durable off. */}
            {!pt.routable && (
              <>
                <DropdownMenuSeparator />
                <DropdownMenuItem variant="destructive" onClick={onRemove}>
                  <Trash2Icon aria-hidden="true" />
                  Delete
                </DropdownMenuItem>
              </>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <dl className="text-muted-foreground grid gap-x-6 gap-y-1 text-sm sm:grid-cols-[auto_1fr]">
        <dt>Replicas</dt>
        <dd>
          {pt.replicas === 0 ? (
            <span className="text-foreground font-medium">0 — paused</span>
          ) : (
            pt.replicas
          )}
        </dd>
        <dt>Health check</dt>
        {pt.resolved_health.inert ? (
          // Still amber, with the warning icon: http with no path means a deploy replaces a
          // working version unverified, and the colour and icon say so at a glance. The owner
          // chose to keep the card short, so the sentence is the tooltip's; a screen-reader
          // copy keeps it available without hovering. (A tooltip is not reachable on touch.)
          <dd data-testid="health-inert" className="min-w-0">
            <Tooltip>
              <TooltipTrigger
                render={
                  <span
                    tabIndex={0}
                    className="inline-flex cursor-help items-center gap-1.5 text-amber-700 dark:text-amber-400"
                  />
                }
              >
                <TriangleAlertIcon
                  aria-hidden="true"
                  className="size-3.5 shrink-0"
                />
                {HEALTH_KIND_LABEL[pt.resolved_health.kind]}
                <span className="sr-only">
                  {" "}
                  — no path set, so nothing is checked. A deploy replaces the
                  running version without checking the new one.
                </span>
              </TooltipTrigger>
              <TooltipPositioner>
                <TooltipContent>
                  {healthText(pt)}. A deploy replaces the running version
                  without checking the new one.
                </TooltipContent>
              </TooltipPositioner>
            </Tooltip>
          </dd>
        ) : (
          <dd className="min-w-0">
            <Tooltip>
              <TooltipTrigger
                render={
                  <span
                    tabIndex={0}
                    className="cursor-help underline decoration-dotted"
                  />
                }
              >
                {healthLabel(pt)}
              </TooltipTrigger>
              <TooltipPositioner>
                <TooltipContent>{healthText(pt)}</TooltipContent>
              </TooltipPositioner>
            </Tooltip>
          </dd>
        )}
      </dl>

      <div className="space-y-1">
        {pt.instances.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            {pt.replicas === 0 ? "Paused — no containers" : "No instance"}
          </p>
        ) : (
          <div
            className={
              expanded && pt.instances.length > EXPANDED_MAX_ROWS
                ? "max-h-80 space-y-1 overflow-y-auto"
                : "space-y-1"
            }
          >
            {shown.map((inst) => (
              <InstanceRow
                key={inst.id}
                inst={inst}
                pt={pt}
                projectId={projectId}
                applicationId={applicationId}
              />
            ))}
          </div>
        )}
        {hidden > 0 && (
          <Button
            variant="ghost"
            size="sm"
            aria-expanded={expanded}
            onClick={() => setExpanded((v) => !v)}
            className="text-muted-foreground"
          >
            <ChevronDownIcon
              aria-hidden="true"
              className={expanded ? "size-4 rotate-180" : "size-4"}
            />
            {expanded ? "Show fewer" : `${hidden} more`}
          </Button>
        )}
      </div>

      {pt.routable && (
        <p className="text-muted-foreground text-xs">
          Domains route to this type, so it cannot be removed.
        </p>
      )}
    </li>
  );
}

/**
 * What reloading one container does, in the order an operator needs it. Reload recreates the
 * container in place from the current image and config: it keeps its name and the route is
 * untouched, and THIS container is gone until its replacement is up.
 *
 * ⛔ Never promises verification. A routable replacement is not health-checked before it
 * takes traffic (a worker's command check, if it has one, is run), and the dialog must not
 * imply otherwise for an application that has configured nothing.
 */
export function ReloadConsequences({
  pt,
  hasVolumes,
}: {
  pt: ProcessType;
  hasVolumes: boolean;
}) {
  const only = pt.instances.length <= 1;
  if (pt.routable && only) {
    // The consequence leads: this is the only web container, so the application is down.
    return (
      <>
        <strong>
          The application will be unavailable while this container restarts.
        </strong>{" "}
        It stops before its replacement starts, and the replacement is not
        health-checked before it takes traffic.
        {hasVolumes
          ? " Reloading the application does the same, because persistent volumes rule out running two copies side by side."
          : " To reload without a gap, use Reload on the application instead: it starts the new container beside the old one and moves traffic when it is ready."}
      </>
    );
  }
  if (pt.routable) {
    return (
      <>
        This container stops before its replacement starts, so it takes no
        traffic meanwhile; the other containers keep serving. The replacement is
        not health-checked before it takes traffic. It picks up the current
        config, and the others keep the old config until they are reloaded or
        the next deploy, so the {pt.name} process runs mixed config until then.
      </>
    );
  }
  return (
    <>
      This container stops before its replacement starts, so it is not running
      meanwhile. The replacement is created from the current image and config.
      {only
        ? ""
        : ` The other ${pt.name} containers keep the old config until they are reloaded or the next deploy.`}
    </>
  );
}

function InstanceRow({
  inst,
  pt,
  projectId,
  applicationId,
}: {
  inst: ProcessInstance;
  pt: ProcessType;
  projectId: string;
  applicationId: string;
}) {
  const restart = useRestartInstance(projectId, applicationId);
  const reload = useReloadInstance(projectId, applicationId);
  const { data: volumes } = useVolumes(projectId, applicationId);
  const [confirmReload, setConfirmReload] = useState(false);
  const isAdmin = useAuthStore((s) => s.user?.role === "admin");
  // No container yet (a type added since the last deploy): nothing to restart or read.
  const hasContainer = inst.container_id !== null;

  const submit = () =>
    toast.promise(restart.mutateAsync(inst.id), {
      loading: `Restarting ${inst.container_name}…`,
      success: (res) =>
        res.status === "restarting"
          ? "Restart in progress — it is taking a while to stop"
          : "Container restarted",
      error: (err) => err.message,
    });

  const submitReload = () => {
    setConfirmReload(false);
    toast.promise(reload.mutateAsync(inst.id), {
      loading: `Reloading ${inst.container_name}…`,
      success: "Reload started — the container is being replaced",
      error: (err) => err.message,
    });
  };

  return (
    <div className="flex min-w-0 items-center justify-between gap-2 rounded-md border px-2 py-1">
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        {/* A word, not just a colour: unhealthy, stopped and error are different facts. */}
        <StatusPill status={inst.status} />
        {/* Short on purpose; the full name is the title and the menu's accessible name. */}
        <span
          title={inst.container_name}
          className="text-text-faint min-w-0 truncate font-mono text-xs"
        >
          {shortContainerName(pt.name, inst)}
        </span>
      </div>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label={`Actions for ${inst.container_name}`}
            />
          }
        >
          <MoreHorizontalIcon aria-hidden="true" className="size-4" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          {hasContainer && inst.container_id && (
            <DropdownMenuItem
              render={
                <Link
                  to="/projects/$projectId/applications/$applicationId/logs"
                  params={{ projectId, applicationId }}
                  search={{ session: inst.container_id }}
                />
              }
            >
              <ScrollTextIcon aria-hidden="true" />
              View Logs
            </DropdownMenuItem>
          )}
          {/* Admin only: the Docker page is restricted to administrators, so a control
              that 403s for the person looking at it must not be rendered. */}
          {isAdmin && (
            <DropdownMenuItem
              render={
                <Link
                  to="/docker"
                  search={{ tab: "containers", q: inst.container_name }}
                />
              }
            >
              <SearchIcon aria-hidden="true" />
              Inspect
            </DropdownMenuItem>
          )}
          <DropdownMenuSeparator />
          <DropdownMenuItem
            disabled={!hasContainer || restart.isPending}
            onClick={submit}
          >
            <RotateCwIcon aria-hidden="true" />
            Restart
          </DropdownMenuItem>
          <DropdownMenuItem
            disabled={!hasContainer || reload.isPending}
            onClick={() => setConfirmReload(true)}
          >
            <RefreshCwIcon aria-hidden="true" />
            Reload
          </DropdownMenuItem>
          {/* No Stop and no Start: see the type menu. */}
        </DropdownMenuContent>
      </DropdownMenu>

      <AlertDialog open={confirmReload} onOpenChange={setConfirmReload}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Reload {inst.container_name}?</AlertDialogTitle>
            <AlertDialogDescription>
              <ReloadConsequences
                pt={pt}
                hasVolumes={(volumes?.length ?? 0) > 0}
              />
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={submitReload}>Reload</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
