import { ShieldCheckIcon, ShieldIcon } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusPill } from "@/components/ui/status-pill";
import {
  describeModuleTab,
  type ModuleTabName,
} from "@/components/proxy/proxy-traffic";
import { useReconcilerStatus } from "@/lib/hooks/use-maintenance";

const TITLE: Record<ModuleTabName, string> = {
  crowdsec: "CrowdSec",
  waf: "Web Application Firewall",
};

const PILL_TONE = {
  neutral: "neutral",
  attention: "building",
  ready: "ready",
} as const;

/** A tab for a module-backed feature that is not built yet: it reports whether
 *  the running Caddy has the module, honouring all three probe states. */
export function ModuleTab({ name }: { name: ModuleTabName }) {
  const { data: status, isLoading } = useReconcilerStatus();
  const state = status?.caddy.optional_modules[name]?.state;
  const d = state ? describeModuleTab(name, state) : null;
  const Icon = state === "available" ? ShieldCheckIcon : ShieldIcon;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          <Icon aria-hidden="true" className="size-4" />
          {TITLE[name]}
          {d && (
            <StatusPill
              status={d.title}
              label={d.title}
              tone={PILL_TONE[d.tone]}
            />
          )}
        </CardTitle>
      </CardHeader>
      <CardContent>
        {isLoading || !d ? (
          <Skeleton className="h-10 w-2/3" />
        ) : (
          <p className="text-muted-foreground text-sm">{d.body}</p>
        )}
      </CardContent>
    </Card>
  );
}
