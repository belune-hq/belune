import { createFileRoute } from "@tanstack/react-router";
import { ProcessTypesSection } from "@/components/applications/process-types-section";
import { useApplication } from "@/lib/hooks/use-applications";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId/processes",
)({
  component: ApplicationProcessesPage,
});

function ApplicationProcessesPage() {
  const { projectId, applicationId } = Route.useParams();
  const { data: application } = useApplication(projectId, applicationId);
  return (
    <ProcessTypesSection
      projectId={projectId}
      applicationId={applicationId}
      appHealthType={application?.health_check_type ?? "http"}
    />
  );
}
