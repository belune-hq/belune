import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { requireAdmin } from "@/lib/utils/auth-guard";
import { SiDocker } from "@icons-pack/react-simple-icons";
import { LayoutDashboard, Box, Layers, HardDrive, Network } from "lucide-react";
import { RouteError } from "@/lib/components/route-error";
import { PageHeader } from "@/components/ui/page-header";
import { PageTabs, type PageTab } from "@/components/ui/page-tabs";
import { useAuthStore } from "@/lib/stores/auth";
import { DockerOverviewTab } from "@/components/docker/overview-tab";
import { DockerContainersTab } from "@/components/docker/containers-tab";
import { DockerImagesTab } from "@/components/docker/images-tab";
import { DockerVolumesTab } from "@/components/docker/volumes-tab";
import { DockerNetworksTab } from "@/components/docker/networks-tab";

type DockerTab = "overview" | "containers" | "images" | "volumes" | "networks";

const TABS: PageTab<DockerTab>[] = [
  { value: "overview", label: "Overview", icon: LayoutDashboard },
  { value: "containers", label: "Containers", icon: Box },
  { value: "images", label: "Images", icon: Layers },
  { value: "volumes", label: "Volumes", icon: HardDrive },
  { value: "networks", label: "Networks", icon: Network },
];

function isTab(v: unknown): v is Exclude<DockerTab, "overview"> {
  return (
    v === "containers" || v === "images" || v === "volumes" || v === "networks"
  );
}

export const Route = createFileRoute("/_app/docker")({
  beforeLoad: requireAdmin,
  component: DockerPage,
  errorComponent: RouteError,
  validateSearch: (search: Record<string, unknown>) => ({
    tab: isTab(search.tab) ? search.tab : undefined,
  }),
});

function DockerPage() {
  const { tab } = Route.useSearch();
  const navigate = useNavigate({ from: "/docker" });
  const isAdmin = useAuthStore((s) => s.user?.role === "admin");

  const activeTab: DockerTab = isTab(tab) ? tab : "overview";
  const setTab = (next: DockerTab) =>
    navigate({
      search: () => ({ tab: next === "overview" ? undefined : next }),
    });

  return (
    <div className="space-y-6">
      <PageHeader
        icon={<SiDocker className="size-5" />}
        title="Docker"
        description="Read-only view of containers, images, volumes, and networks on this host."
      />

      {!isAdmin ? (
        <p className="text-muted-foreground text-sm">
          Docker inspection is restricted to administrators.
        </p>
      ) : (
        <>
          <PageTabs
            ariaLabel="Docker views"
            items={TABS}
            value={activeTab}
            onValueChange={setTab}
          />

          {activeTab === "overview" && (
            <DockerOverviewTab enabled={activeTab === "overview"} />
          )}
          {activeTab === "containers" && (
            <DockerContainersTab enabled={activeTab === "containers"} />
          )}
          {activeTab === "images" && (
            <DockerImagesTab enabled={activeTab === "images"} />
          )}
          {activeTab === "volumes" && (
            <DockerVolumesTab enabled={activeTab === "volumes"} />
          )}
          {activeTab === "networks" && (
            <DockerNetworksTab enabled={activeTab === "networks"} />
          )}
        </>
      )}
    </div>
  );
}
