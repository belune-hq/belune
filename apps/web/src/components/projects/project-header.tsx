import { LayersIcon } from "lucide-react";
import { StatusPill } from "@/components/ui/status-pill";
import type { Application, Database, Project } from "@/lib/types";

interface Props {
  project?: Project;
  applications?: Application[];
  databases?: Database[];
}

/** Two uppercase initials derived from the project name. */
function initials(name: string): string {
  const parts = name
    .trim()
    .split(/[\s-_]+/)
    .filter(Boolean);
  if (parts.length === 0) return "··";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[1][0]).toUpperCase();
}

const ERROR_STATES = new Set([
  "failed",
  "error",
  "crashed",
  "unhealthy",
  "exited",
]);

export function ProjectHeader({ project, applications, databases }: Props) {
  const services = [
    ...(applications ?? []).map((a) => a.status),
    ...(databases ?? []).map((d) => d.status),
  ];
  const total = services.length;
  const running = services.filter(
    (s) => s.toLowerCase() === "running" || s.toLowerCase() === "ready",
  ).length;
  const errored = services.filter((s) =>
    ERROR_STATES.has(s.toLowerCase()),
  ).length;

  const name = project?.name ?? "Project";

  return (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div className="flex min-w-0 items-center gap-3">
        <div
          aria-hidden="true"
          className="bg-elev text-text-muted grid size-11 shrink-0 place-items-center rounded-xl text-sm font-semibold"
        >
          {initials(name)}
        </div>
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="truncate text-2xl font-semibold tracking-tight">
              {name}
            </h1>
            {total > 0 && (
              <StatusPill
                status="running"
                // Colour reflects the running ratio, not the word "running":
                // all up → brand, none up → red, and anything in between → the
                // amber (status-building) tone as a degraded signal.
                tone={
                  running === total
                    ? "ready"
                    : running === 0
                      ? "error"
                      : "building"
                }
                // Steady dot: this is a settled count, not an in-progress state.
                pulse={false}
                label={`${running}/${total} running`}
              />
            )}
            {errored > 0 && (
              <StatusPill
                status="error"
                label={`${errored} error${errored === 1 ? "" : "s"}`}
              />
            )}
          </div>
          {project && (
            <div className="text-text-faint flex items-center gap-3 text-sm">
              <span className="truncate font-mono">{project.slug}</span>
              <span className="inline-flex items-center gap-1">
                <LayersIcon aria-hidden="true" className="size-3.5" />
                {total} service{total === 1 ? "" : "s"}
              </span>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
