import { createFileRoute } from "@tanstack/react-router";
import { Loader2Icon } from "lucide-react";
import {
  useDeployments,
  useRollbackDeployment,
} from "@/lib/hooks/use-deployments";
import { useApplication } from "@/lib/hooks/use-applications";
import { AutoDeploySection } from "@/components/applications/auto-deploy-section";
import { useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "@/lib/hooks/query-keys";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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
import type { Deployment } from "@/lib/types";
import {
  formatDateTime,
  formatDateTimeShort,
  formatDuration,
} from "@/lib/utils/format";
import { useState, useCallback, useRef } from "react";
import { useChannel } from "@/lib/hooks/use-websocket";
import { BlobLogViewer } from "@/components/logs/blob-log-viewer";
import {
  parseLogBlob,
  stripAnsi,
  type LogEntry,
} from "@/components/logs/parse";
import { normalizeLevel } from "@/lib/logs/level";
import { RouteError } from "@/lib/components/route-error";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId/deployments",
)({
  component: DeploymentsPage,
  errorComponent: RouteError,
});

const statusVariant: Record<
  string,
  "default" | "secondary" | "destructive" | "outline" | "light"
> = {
  success: "light",
  pending: "secondary",
  building: "secondary",
  deploying: "secondary",
  failed: "destructive",
};

// Live build-log lines are kept in a module-level cache keyed by deployment so
// that navigating away from the Deployments tab and back restores what was
// already streamed (component state would otherwise reset to empty, and Redis
// pub/sub does not replay past messages). Cleared when the build finishes.
const liveBuildLogCache = new Map<string, LogEntry[]>();

function useBuildLogStream(
  projectId: string,
  applicationId: string,
  deploymentId: string,
  isBuilding: boolean,
  seedBlob: string | null | undefined,
) {
  const [entries, setEntries] = useState<LogEntry[]>(() => {
    const cached = liveBuildLogCache.get(deploymentId);
    if (cached) return cached;
    // After a page refresh the module cache is empty and Redis pub/sub can't
    // replay past lines. Seed from the partial build_logs the worker flushes
    // every ~2s during the build, so the log isn't blank ("Waiting for
    // output...") until the next live line arrives or the build finishes.
    return parseLogBlob(seedBlob ?? "");
  });
  const idRef = useRef(entries.length);
  const queryClient = useQueryClient();

  const handleMessage = useCallback(
    (event: string, data: unknown) => {
      if (event === "done") {
        liveBuildLogCache.delete(deploymentId);
        queryClient.invalidateQueries({
          queryKey: queryKeys.deployments.all(projectId, applicationId),
        });
        return;
      }
      // Each message is one NDJSON log entry: { ts, level, msg }.
      if (!data || typeof data !== "object") return;
      const obj = data as { ts?: string; level?: string; msg?: string };
      if (typeof obj.msg !== "string") return;
      idRef.current += 1;
      setEntries((prev) => {
        const next = [
          ...prev,
          {
            id: `build-${idRef.current}`,
            level: normalizeLevel(obj.level ?? "info"),
            // Live build output (railpack/npm/etc.) is colourised; the completed
            // blob path is stripped by parseLogBlob, so strip the live one to match.
            message: stripAnsi(obj.msg as string),
            recordedAt: obj.ts ?? null,
          },
        ];
        liveBuildLogCache.set(deploymentId, next);
        return next;
      });
    },
    [queryClient, projectId, applicationId, deploymentId],
  );

  const channel = isBuilding ? `build-logs:${deploymentId}` : null;
  useChannel(channel, handleMessage);

  return entries;
}

function BuildLogViewer({ entries }: { entries: LogEntry[] }) {
  return (
    <BlobLogViewer
      className="mt-3"
      entries={entries}
      running
      emptyMessage="Waiting for build output..."
    />
  );
}

function RollbackButton({
  deployment,
  projectId,
  applicationId,
}: {
  deployment: Deployment;
  projectId: string;
  applicationId: string;
}) {
  const { mutate: rollback, isPending } = useRollbackDeployment(
    projectId,
    applicationId,
  );

  return (
    <AlertDialog>
      <AlertDialogTrigger
        render={
          <Button
            size="sm"
            variant="outline"
            disabled={isPending}
            onClick={(e) => e.stopPropagation()}
          />
        }
      >
        {isPending ? "Rolling back..." : "Rollback"}
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Rollback to this deployment?</AlertDialogTitle>
          <AlertDialogDescription>
            This will redeploy the image from{" "}
            <strong>{formatDateTimeShort(deployment.started_at)}</strong>
            {deployment.commit_sha && (
              <>
                {" "}
                (commit <code>{deployment.commit_sha.slice(0, 7)}</code>)
              </>
            )}
            . A new deployment will be created with the stored image tag.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel onClick={(e) => e.stopPropagation()}>
            Cancel
          </AlertDialogCancel>
          <AlertDialogAction
            onClick={(e) => {
              e.stopPropagation();
              rollback(deployment.id);
            }}
          >
            Rollback
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function DeploymentCard({
  deployment: d,
  projectId,
  applicationId,
}: {
  deployment: Deployment;
  projectId: string;
  applicationId: string;
}) {
  const [expanded, setExpanded] = useState(false);
  const isBuilding = d.status === "building" || d.status === "pending";
  // A deployment has no meaningful duration until it finishes; while it runs,
  // the slot shows a spinner instead of a stale or blank duration.
  const inProgress =
    d.status === "pending" ||
    d.status === "building" ||
    d.status === "deploying";
  const liveEntries = useBuildLogStream(
    projectId,
    applicationId,
    d.id,
    isBuilding,
    d.build_logs,
  );
  const canRollback = d.status === "success" && !!d.image_tag;

  // Overall duration (started → finished)
  const totalDuration =
    d.finished_at && d.started_at
      ? formatDuration(
          new Date(d.finished_at).getTime() - new Date(d.started_at).getTime(),
        )
      : null;

  // Build phase: build_started_at → build_ended_at
  const buildDuration =
    d.build_started_at && d.build_ended_at
      ? formatDuration(
          new Date(d.build_ended_at).getTime() -
            new Date(d.build_started_at).getTime(),
        )
      : null;

  // Deploy phase: deploy_started_at → finished_at
  const deployDuration =
    d.deploy_started_at && d.finished_at
      ? formatDuration(
          new Date(d.finished_at).getTime() -
            new Date(d.deploy_started_at).getTime(),
        )
      : null;

  const hasSplit = buildDuration !== null && deployDuration !== null;

  return (
    <Card
      className="hover:bg-muted/50 cursor-pointer transition-colors"
      onClick={() => setExpanded(!expanded)}
    >
      <CardContent className="py-3">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-3">
            <Badge
              variant={statusVariant[d.status] ?? "outline"}
              className="capitalize"
            >
              {d.status}
            </Badge>
            <span className="text-muted-foreground text-sm">
              {d.triggered_by}
            </span>
            {d.commit_sha && (
              <span className="text-muted-foreground font-mono text-xs">
                {d.commit_sha.slice(0, 7)}
              </span>
            )}
            {d.commit_message && (
              // Only the subject line: commit bodies are multi-paragraph and
              // this row is a single line.
              <span className="text-muted-foreground min-w-0 truncate text-xs">
                {d.commit_message.split("\n")[0]}
                {d.commit_author && (
                  <span className="text-text-faint"> · {d.commit_author}</span>
                )}
              </span>
            )}
          </div>
          <div className="flex items-center gap-3">
            {canRollback && (
              <RollbackButton
                deployment={d}
                projectId={projectId}
                applicationId={applicationId}
              />
            )}
            <div className="text-muted-foreground flex items-center gap-3 text-xs">
              {inProgress ? (
                <Loader2Icon
                  aria-label="In progress"
                  className="size-3.5 animate-spin"
                />
              ) : hasSplit ? (
                <span
                  title={`Build: ${buildDuration} · Deploy: ${deployDuration}`}
                >
                  {buildDuration} + {deployDuration}
                </span>
              ) : totalDuration ? (
                <span>{totalDuration}</span>
              ) : null}
              <span>{formatDateTime(d.started_at)}</span>
            </div>
          </div>
        </div>
        {expanded && (
          // Stop clicks inside the log area (level filter, wrap toggle, text
          // selection) from bubbling up to the card and collapsing it.
          <div onClick={(e) => e.stopPropagation()}>
            {isBuilding && <BuildLogViewer entries={liveEntries} />}
            {!isBuilding && d.build_logs && (
              // Open at the bottom so it stays where the live viewer left off
              // when the build finishes.
              <BlobLogViewer className="mt-3" blob={d.build_logs} follow />
            )}
            {d.error_message && (
              <div className="bg-destructive/10 text-destructive mt-3 rounded p-3 text-sm">
                {d.error_message}
              </div>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function DeploymentsPage() {
  const { projectId, applicationId } = Route.useParams();
  const { data: application } = useApplication(projectId, applicationId);

  return (
    <div className="space-y-6">
      {/* How this app deploys itself sits above the record of what it has
          deployed. Rendered outside DeploymentHistory so it is still reachable
          when there is no history yet — which is exactly when you want it. */}
      {application && (
        <AutoDeploySection
          projectId={projectId}
          applicationId={applicationId}
          application={application}
        />
      )}
      <DeploymentHistory projectId={projectId} applicationId={applicationId} />
    </div>
  );
}

const DEPLOYMENTS_PER_PAGE = 10;

function DeploymentHistory({
  projectId,
  applicationId,
}: {
  projectId: string;
  applicationId: string;
}) {
  const {
    data: deployments,
    isLoading,
    error,
  } = useDeployments(projectId, applicationId);
  const [page, setPage] = useState(0);

  if (isLoading) {
    return (
      <div className="space-y-3">
        {[1, 2, 3].map((i) => (
          <Card key={i}>
            <CardContent className="py-3">
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-3">
                  <Skeleton className="h-5 w-16 rounded-full" />
                  <Skeleton className="h-4 w-20" />
                </div>
                <Skeleton className="h-4 w-32" />
              </div>
            </CardContent>
          </Card>
        ))}
      </div>
    );
  }

  if (error) {
    return (
      <Card>
        <CardContent className="text-destructive py-12 text-center">
          Failed to load deployments: {error.message}
        </CardContent>
      </Card>
    );
  }

  if (!deployments || deployments.length === 0) {
    return (
      <Card>
        <CardContent className="text-muted-foreground py-12 text-center">
          No deployments yet. Deploy your application to see history here.
        </CardContent>
      </Card>
    );
  }

  const total = deployments.length;
  const totalPages = Math.ceil(total / DEPLOYMENTS_PER_PAGE);
  // Clamp rather than trust `page`: a live update (WebSocket invalidation) can
  // shrink the list under a page the user had scrolled to.
  const safePage = Math.min(page, totalPages - 1);
  const start = safePage * DEPLOYMENTS_PER_PAGE;
  const pageItems = deployments.slice(start, start + DEPLOYMENTS_PER_PAGE);

  return (
    <div className="space-y-3">
      {pageItems.map((d) => (
        <DeploymentCard
          key={d.id}
          deployment={d}
          projectId={projectId}
          applicationId={applicationId}
        />
      ))}
      {total > DEPLOYMENTS_PER_PAGE && (
        <div className="flex items-center justify-between pt-1">
          <Button
            variant="outline"
            size="sm"
            disabled={safePage <= 0}
            onClick={() => setPage(safePage - 1)}
          >
            Previous
          </Button>
          <span className="text-muted-foreground text-sm">
            {start + 1}–{Math.min(total, start + DEPLOYMENTS_PER_PAGE)} of{" "}
            {total}
          </span>
          <Button
            variant="outline"
            size="sm"
            disabled={safePage >= totalPages - 1}
            onClick={() => setPage(safePage + 1)}
          >
            Next
          </Button>
        </div>
      )}
    </div>
  );
}
