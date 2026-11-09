import { useState } from "react";
import { toast } from "sonner";
import {
  BoxesIcon,
  InfoIcon,
  PencilIcon,
  RotateCwIcon,
  PlusIcon,
  Trash2Icon,
  TriangleAlertIcon,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { IconAction } from "@/components/ui/icon-action";
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
  useRestartInstance,
} from "@/lib/hooks/use-process-types";
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
              Process Types
            </CardTitle>
            <CardDescription className="mt-1">
              Run a different command from the same built image — a queue worker
              or a scheduler beside the web process. Each type gets its own
              container. Only the routable type receives traffic.
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
          <ul className="space-y-3">
            {types.map((pt) => (
              <TypeRow
                key={pt.id}
                pt={pt}
                projectId={projectId}
                applicationId={applicationId}
                onEdit={() => {
                  setEditing(pt);
                  setFormOpen(true);
                }}
                onRemove={() => setRemoveTarget(pt)}
              />
            ))}
          </ul>
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

// What the type is limited to. A NULL limit is "inherit the application's", said
// outright: a blank would read as unset-and-therefore-unlimited.
function limitsText(pt: ProcessType): string {
  const cpu =
    pt.cpu_limit != null ? `CPU ${pt.cpu_limit} cores` : "CPU inherited";
  const mem =
    pt.memory_limit != null
      ? `Memory ${Math.round(pt.memory_limit / (1024 * 1024))} MB`
      : "Memory inherited";
  return `${cpu} · ${mem}`;
}

function healthText(pt: ProcessType): string {
  const h = pt.resolved_health;
  const label = { http: "HTTP probe", command: "Command", none: "None" }[
    h.kind
  ];
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

function TypeRow({
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
  return (
    <li className="space-y-2 rounded-lg border p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="truncate font-mono font-medium">{pt.name}</span>
          {pt.routable && (
            <span className="border-primary/20 bg-primary/10 text-brand-text rounded-full border px-2 py-0.5 text-xs font-medium">
              Routable
            </span>
          )}
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <IconAction label={`Edit ${pt.name}`} onClick={onEdit}>
            <PencilIcon aria-hidden="true" className="size-4" />
          </IconAction>
          {pt.routable ? null : (
            <IconAction
              label={`Remove ${pt.name}`}
              destructive
              onClick={onRemove}
            >
              <Trash2Icon aria-hidden="true" className="size-4" />
            </IconAction>
          )}
        </div>
      </div>

      <dl className="text-muted-foreground grid gap-x-6 gap-y-1 text-sm sm:grid-cols-[auto_1fr]">
        <dt>Command</dt>
        <dd className="min-w-0 truncate font-mono text-xs leading-5">
          {pt.command ? (
            pt.command
          ) : (
            <span className="font-sans text-sm">the image&apos;s own</span>
          )}
        </dd>
        <dt>Replicas</dt>
        <dd>
          {pt.replicas === 0 ? (
            <span className="text-foreground font-medium">0 — paused</span>
          ) : (
            pt.replicas
          )}
        </dd>
        <dt>Resources</dt>
        <dd className="min-w-0 truncate">{limitsText(pt)}</dd>
        <dt>Health check</dt>
        {pt.resolved_health.inert ? (
          <dd
            className="flex min-w-0 items-start gap-1.5 text-amber-700 dark:text-amber-400"
            data-testid="health-inert"
          >
            <TriangleAlertIcon
              aria-hidden="true"
              className="mt-0.5 size-3.5 shrink-0"
            />
            <span>
              {healthText(pt)}. A deploy replaces the running version without
              checking the new one.
            </span>
          </dd>
        ) : (
          <dd className="min-w-0 truncate">{healthText(pt)}</dd>
        )}
        <dt>Container</dt>
        <dd className="space-y-1">
          {pt.instances.length === 0 ? (
            <span>
              {pt.replicas === 0 ? "Paused — no containers" : "No instance"}
            </span>
          ) : (
            pt.instances.map((inst) => (
              <InstanceRow
                key={inst.id}
                inst={inst}
                projectId={projectId}
                applicationId={applicationId}
              />
            ))
          )}
        </dd>
      </dl>

      {pt.routable && (
        <p className="text-muted-foreground text-xs">
          Domains route to this type, so it cannot be removed.
        </p>
      )}
    </li>
  );
}

function InstanceRow({
  inst,
  projectId,
  applicationId,
}: {
  inst: ProcessInstance;
  projectId: string;
  applicationId: string;
}) {
  const restart = useRestartInstance(projectId, applicationId);
  // No container yet (a type added since the last deploy): nothing to restart.
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

  return (
    <div className="flex min-w-0 flex-wrap items-center gap-2">
      <span className="text-text-faint min-w-0 truncate font-mono text-xs">
        {inst.container_name}
      </span>
      <StatusPill status={inst.status} />
      <IconAction
        label={`Restart ${inst.container_name}`}
        onClick={submit}
        disabled={!hasContainer || restart.isPending}
      >
        <RotateCwIcon aria-hidden="true" className="size-4" />
      </IconAction>
    </div>
  );
}
