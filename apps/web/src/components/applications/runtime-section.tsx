import { useState } from "react";
import { PendingChangeBadge } from "@/lib/components/pending-change-badge";
import { ShieldCheck } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { Label } from "@/components/ui/label";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "@/components/ui/segmented-control";
import { useUpdateApplicationRuntime } from "@/lib/hooks/use-applications";
import type { Application } from "@/lib/types";

interface Props {
  projectId: string;
  applicationId: string;
  application: Application;
}

/**
 * Container runtime profile: read-only rootfs + capability set. Untrusted apps
 * (git builds, manually-added images) default to hardened; curated template
 * apps default to standard. Changes are staged locally and applied on Save.
 * Saving stamps the config-changed marker, so the header badge then says how to
 * apply it — a reload is enough, since the profile is applied when the
 * container is created, not when the image is built.
 */
export function RuntimeSection({
  projectId,
  applicationId,
  application,
}: Props) {
  const update = useUpdateApplicationRuntime(projectId, applicationId);

  const [readonly, setReadonly] = useState(application.readonly_rootfs);
  const [caps, setCaps] = useState<"minimal" | "standard">(
    application.container_caps,
  );

  // Resync staged values when the persisted ones change (e.g. after a save).
  const sig = `${application.readonly_rootfs}:${application.container_caps}`;
  const [syncedSig, setSyncedSig] = useState(sig);
  if (sig !== syncedSig) {
    setSyncedSig(sig);
    setReadonly(application.readonly_rootfs);
    setCaps(application.container_caps);
  }

  const dirty =
    readonly !== application.readonly_rootfs ||
    caps !== application.container_caps;

  const save = () =>
    update.mutate({ readonly_rootfs: readonly, container_caps: caps });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheck aria-hidden="true" className="size-4" />
          Runtime
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-5">
        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0">
            <Label htmlFor="ro-rootfs" className="font-medium">
              Read-only root filesystem
            </Label>
            <p className="text-muted-foreground mt-0.5 text-xs">
              Blocks writes to the container filesystem except mounted volumes
              and <code>/tmp</code>, <code>/run</code>. Some stock images need
              this off.
            </p>
          </div>
          <Switch
            id="ro-rootfs"
            aria-label="Read-only root filesystem"
            checked={readonly}
            disabled={update.isPending}
            onCheckedChange={setReadonly}
          />
        </div>

        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0">
            <Label className="font-medium">Capabilities</Label>
            <p className="text-muted-foreground mt-0.5 text-xs">
              <strong>Minimal</strong> drops all Linux capabilities (most
              hardened). <strong>Standard</strong> grants Docker's default set —
              needed by images that change file ownership or switch users at
              startup.
            </p>
          </div>
          <SegmentedControl
            value={caps}
            onValueChange={(v) =>
              setCaps(v === "standard" ? "standard" : "minimal")
            }
            size="sm"
          >
            <SegmentedControlItem value="minimal" disabled={update.isPending}>
              Minimal
            </SegmentedControlItem>
            <SegmentedControlItem value="standard" disabled={update.isPending}>
              Standard
            </SegmentedControlItem>
          </SegmentedControl>
        </div>

        <div className="flex items-center justify-between gap-4">
          <div className="text-text-faint flex items-center gap-2 text-xs">
            <span>
              Applied when the container is next recreated.
              {application.source_kind === "template" &&
                " This app was created from a template."}
            </span>
            <PendingChangeBadge app={application} />
          </div>
          <Button onClick={save} disabled={!dirty || update.isPending}>
            {update.isPending ? "Saving…" : "Save"}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
