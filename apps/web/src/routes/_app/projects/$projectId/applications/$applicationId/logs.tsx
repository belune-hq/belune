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
  const { data: processTypes } = useProcessTypes(projectId, applicationId);
  // The viewer is shared with databases and does not know which type is routable. The
  // combined view is readable now and stays one click away; only the default is the
  // type an operator is usually asking about.
  const routable = processTypes?.find((t) => t.routable)?.name;
  return (
    <ContainerLogViewer
      source="application"
      projectId={projectId}
      sourceId={applicationId}
      defaultProcessType={routable}
    />
  );
}
