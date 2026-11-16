import { createFileRoute } from "@tanstack/react-router";
import { ContainerLogViewer } from "@/components/logs/container-log-viewer";
import { useProcessTypes } from "@/lib/hooks/use-process-types";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId/logs",
)({
  component: LogsPage,
});

function LogsPage() {
  const { projectId, applicationId } = Route.useParams();
  const { data: processTypes, isPending } = useProcessTypes(
    projectId,
    applicationId,
  );
  // The viewer is shared with databases and does not know which type is routable. The
  // combined view is readable now and stays one click away; only the default is the
  // type an operator is usually asking about.
  const routable = processTypes?.find((t) => t.routable)?.name;

  // Hold the viewer until the types resolve. Mounted earlier it reads "no default" and
  // fires an UNFILTERED history request, then a second one when the types arrive: a wasted
  // round trip and a flash of the combined view, the thing the default exists to avoid.
  // Only while pending: a failed lookup falls through to the combined view, not a spinner.
  if (isPending) {
    return (
      <div
        role="status"
        aria-label="Loading logs"
        className="bg-muted/40 mt-3 h-[600px] animate-pulse rounded-lg"
      />
    );
  }
  return (
    <ContainerLogViewer
      source="application"
      projectId={projectId}
      sourceId={applicationId}
      defaultProcessType={routable}
    />
  );
}
