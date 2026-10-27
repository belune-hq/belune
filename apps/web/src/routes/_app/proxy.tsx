import { createFileRoute, useNavigate } from "@tanstack/react-router";
import {
  BrickWallShieldIcon,
  RouteIcon,
  ShieldCheckIcon,
  ShieldIcon,
} from "lucide-react";
import { PageHeader } from "@/components/ui/page-header";
import { PageTabs, type PageTab } from "@/components/ui/page-tabs";
import { CaddyTab } from "@/components/proxy/caddy-tab";
import { CrowdSecTab } from "@/components/proxy/crowdsec-tab";
import { isCrowdSecEnforcing } from "@/components/proxy/crowdsec-view";
import { useCrowdSecStatus } from "@/lib/hooks/use-crowdsec";
import { WafTab } from "@/components/proxy/waf-tab";
import { RouteError } from "@/lib/components/route-error";
import { requireAdmin } from "@/lib/utils/auth-guard";

type ProxyTab = "caddy" | "crowdsec" | "waf";

// The CrowdSec tab's glyph reports whether it is enforcing, which is the one
// piece of state readable from the other two tabs — there is no pill out here.
// The rule lives in crowdsec-view so the tab and the card cannot disagree.
const proxyTabs = (enforcing: boolean): PageTab<ProxyTab>[] => [
  { value: "caddy", label: "Caddy", icon: RouteIcon },
  {
    value: "crowdsec",
    label: "CrowdSec",
    icon: enforcing ? ShieldCheckIcon : ShieldIcon,
  },
  { value: "waf", label: "WAF", icon: BrickWallShieldIcon },
];

export const Route = createFileRoute("/_app/proxy")({
  beforeLoad: requireAdmin,
  component: ProxyPage,
  errorComponent: RouteError,
  validateSearch: (search: Record<string, unknown>) => ({
    tab:
      search.tab === "crowdsec" || search.tab === "waf"
        ? (search.tab as ProxyTab)
        : undefined,
  }),
});

function ProxyPage() {
  const { tab } = Route.useSearch();
  const navigate = useNavigate({ from: "/proxy" });
  const activeTab: ProxyTab = tab ?? "caddy";
  // Shares the CrowdSec tab's own query key, so this is the same request that
  // tab already makes rather than a second one.
  const crowdsec = useCrowdSecStatus();
  const tabs = proxyTabs(isCrowdSecEnforcing(crowdsec.data));
  const setTab = (next: ProxyTab) =>
    navigate({ search: () => ({ tab: next === "caddy" ? undefined : next }) });

  return (
    <div className="space-y-6">
      <PageHeader
        icon={<RouteIcon className="size-5" />}
        title="Proxy"
        description="The reverse proxy in front of every app: its health, traffic and modules."
      />

      <PageTabs
        ariaLabel="Proxy views"
        items={tabs}
        value={activeTab}
        onValueChange={setTab}
      />

      {activeTab === "caddy" ? (
        <CaddyTab />
      ) : activeTab === "waf" ? (
        <WafTab />
      ) : (
        <CrowdSecTab />
      )}
    </div>
  );
}
