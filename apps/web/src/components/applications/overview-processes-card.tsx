import { Link } from "@tanstack/react-router";
import { BoxesIcon } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusPill } from "@/components/ui/status-pill";
import { useProcessTypes } from "@/lib/hooks/use-process-types";
import { summariseProcess } from "@/lib/utils/process-usage";

/**
 * The application as N containers across M process types, with a status per type. The
 * header's single pill is one value for all of them: with web up and a worker down it
 * says "Running", which is a guess. This is what the Overview shows instead of making
 * the reader open the Processes tab to find out.
 */
export function OverviewProcessesCard({
  projectId,
  applicationId,
}: {
  projectId: string;
  applicationId: string;
}) {
  const { data: types, isLoading } = useProcessTypes(projectId, applicationId);

  const containers = (types ?? []).reduce((n, t) => n + t.instances.length, 0);
  const summary = types
    ? `${containers} ${containers === 1 ? "container" : "containers"} across ${types.length} ${types.length === 1 ? "process" : "processes"}`
    : null;

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <CardTitle className="flex items-center gap-2">
            <BoxesIcon aria-hidden="true" className="size-4" />
            Processes
          </CardTitle>
          <div className="text-text-faint flex items-center gap-3 text-xs">
            {summary && <span>{summary}</span>}
            <Link
              to="/projects/$projectId/applications/$applicationId/processes"
              params={{ projectId, applicationId }}
              className="hover:text-foreground underline"
            >
              Manage
            </Link>
          </div>
        </div>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-10 w-full" />
        ) : !types || types.length === 0 ? (
          <p className="text-muted-foreground py-2 text-center text-sm">
            No processes.
          </p>
        ) : (
          <ul className="divide-border divide-y">
            {types.map((t) => {
              const status = summariseProcess(t);
              return (
                <li
                  key={t.id}
                  className="flex items-center justify-between gap-3 py-2.5 text-sm"
                >
                  <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
                    <span className="font-mono font-medium">{t.name}</span>
                    <span className="text-text-faint text-xs">
                      ×{t.replicas}
                    </span>
                    {t.routable && (
                      <span className="text-brand-text text-xs">
                        receives traffic
                      </span>
                    )}
                  </div>
                  <StatusPill
                    status={status.label}
                    label={status.label}
                    tone={status.tone}
                    pulse={false}
                  />
                </li>
              );
            })}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}
