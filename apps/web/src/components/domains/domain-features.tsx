import { useId, useState } from "react";
import { toast } from "sonner";
import {
  ArrowRightIcon,
  GaugeIcon,
  ListIcon,
  LockIcon,
  ShieldIcon,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CodeEditor } from "@/components/ui/code-editor";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  useDomains,
  useUpsertRouteFeature,
  useDeleteRouteFeature,
} from "@/lib/hooks/use-domains";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Tooltip,
  TooltipContent,
  TooltipPositioner,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { Trash2Icon } from "lucide-react";
import { useProxyCapabilities } from "@/lib/hooks/use-proxy-capabilities";
import {
  featureStatus,
  inertExplanation,
  type FeatureStatus,
} from "@/lib/route-feature-status";
import type { DomainExpanded, RouteFeature } from "@/lib/types";

const FEATURE_TYPES = [
  { value: "basic_auth", label: "Basic Auth", Icon: LockIcon },
  { value: "headers", label: "Custom Headers", Icon: ListIcon },
  { value: "ip_allowlist", label: "IP Allowlist", Icon: ShieldIcon },
  { value: "redirect", label: "Redirect", Icon: ArrowRightIcon },
  { value: "rate_limit", label: "Rate Limit", Icon: GaugeIcon },
] as const;

// Prefilled when a type is picked, so nobody has to guess the shape. Only types
// whose config is not obvious from "{}" need an entry.
const CONFIG_EXAMPLES: Record<string, string> = {
  rate_limit: '{"window":"1s","max_events":10}',
};

/**
 * Route features for one domain — middleware Caddy applies to the route.
 *
 * These save on their own, not with the dialog's Save Changes: each one is an
 * immediate upsert or delete against its own endpoint. That is deliberate but
 * worth knowing, and it is why the tab only exists when editing an existing
 * domain — a domain that has not been created yet has no id to attach them to.
 */
export function DomainFeatures({
  projectId,
  applicationId,
  domain,
}: {
  projectId: string;
  applicationId: string;
  domain: DomainExpanded;
}) {
  // Read the features from the query, not from the `domain` prop.
  //
  // The prop is a snapshot taken when Edit was clicked and it never updates: the
  // page holds the domain in useState, so adding a feature invalidated the query,
  // refetched the list, and left this dialog still rendering the object from
  // before — reporting "No route features configured" for a feature that had in
  // fact been created, saved, and pushed to Caddy. The mutations below already
  // invalidate this query, so reading from it makes the list live.
  const { data: domains } = useDomains(projectId, applicationId);
  const live = domains?.find((d) => d.id === domain.id) ?? domain;

  const upsertFeature = useUpsertRouteFeature(projectId, applicationId);
  const deleteFeature = useDeleteRouteFeature(projectId, applicationId);
  const [newFeatureType, setNewFeatureType] = useState("");
  const [newFeatureConfig, setNewFeatureConfig] = useState("{}");
  // A <label for> cannot reach the editor's contenteditable, so the label is
  // tied to it by id instead.
  const configLabelId = useId();
  // `undefined` while loading or if the request failed: both mean "unknown", and
  // featureStatus makes no claim about the operator's Caddy in that case.
  const { data: capabilities } = useProxyCapabilities();

  const handleAddFeature = () => {
    if (!newFeatureType) return;
    let config: Record<string, unknown>;
    try {
      config = JSON.parse(newFeatureConfig);
    } catch {
      toast.error("Invalid JSON config");
      return;
    }
    toast.promise(
      upsertFeature
        .mutateAsync({
          domainId: domain.id,
          feature_type: newFeatureType,
          config,
          enabled: true,
        })
        .then(() => {
          setNewFeatureType("");
          setNewFeatureConfig("{}");
        }),
      {
        loading: "Adding feature…",
        success: "Feature added",
        error: (err) => err.message,
      },
    );
  };

  const features = live.route_features ?? [];

  return (
    <div className="min-w-0 space-y-3">
      {features.length > 0 ? (
        <div className="space-y-2">
          {features.map((feature) => (
            <FeatureRow
              key={feature.id}
              feature={feature}
              status={featureStatus(feature, capabilities, live.not_applied)}
              onDelete={() => {
                toast.promise(
                  deleteFeature.mutateAsync({
                    domainId: domain.id,
                    featureId: feature.id,
                  }),
                  {
                    loading: "Removing…",
                    success: "Feature removed",
                    error: (err) => err.message,
                  },
                );
              }}
              onToggle={(enabled) => {
                toast.promise(
                  upsertFeature.mutateAsync({
                    domainId: domain.id,
                    feature_type: feature.feature_type,
                    config: feature.config as Record<string, unknown>,
                    enabled,
                  }),
                  {
                    loading: "Updating…",
                    success: enabled ? "Feature enabled" : "Feature disabled",
                    error: (err) => err.message,
                  },
                );
              }}
            />
          ))}
        </div>
      ) : (
        <p className="text-muted-foreground text-xs">
          No route features configured.
        </p>
      )}

      <div className="space-y-2 border-t pt-3">
        <div className="space-y-1">
          <Label className="text-xs">Type</Label>
          <Select
            value={newFeatureType}
            onValueChange={(value) => {
              const next = value ?? "";
              setNewFeatureType(next);
              // Replace only the untouched placeholder, never something typed.
              if (newFeatureConfig === "{}" || newFeatureConfig === "") {
                setNewFeatureConfig(CONFIG_EXAMPLES[next] ?? "{}");
              }
            }}
          >
            <SelectTrigger className="w-full capitalize">
              <SelectValue placeholder="Select…" />
            </SelectTrigger>
            <SelectContent>
              {FEATURE_TYPES.map((ft) => {
                // Offered but disabled, with the reason: a type that vanished
                // from the list would read as a bug in Belune. Only a probe that
                // SAW the module set can disable it; unknown leaves it open.
                const need = capabilities?.features[ft.value];
                const missing = need?.state === "unavailable";
                return (
                  <SelectItem
                    key={ft.value}
                    value={ft.value}
                    icon={<ft.Icon />}
                    className="capitalize"
                    disabled={missing}
                  >
                    {ft.label}
                    {missing && (
                      <span className="text-muted-foreground ml-2 text-xs normal-case">
                        your Caddy does not have the {need.module} module
                      </span>
                    )}
                  </SelectItem>
                );
              })}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1">
          <Label id={configLabelId} className="text-xs">
            Config (JSON)
          </Label>
          {/* Highlighting and bracket matching only. What counts as a valid
              config is still JSON.parse in handleAddFeature and then the
              server's ParseFeatureConfig: the editor must not grow opinions
              (max_events: 0 is a footgun the SERVER rejects, and colouring or
              flagging it here would be a second, unauthoritative verdict). */}
          <CodeEditor
            compact
            language="json"
            value={newFeatureConfig}
            onChange={setNewFeatureConfig}
            placeholder={CONFIG_EXAMPLES[newFeatureType] ?? '{"key": "value"}'}
            aria-labelledby={configLabelId}
          />
        </div>
        <Button
          size="sm"
          variant="outline"
          className="w-full"
          onClick={handleAddFeature}
          disabled={!newFeatureType || upsertFeature.isPending}
        >
          Add feature
        </Button>
      </div>
    </div>
  );
}

function FeatureRow({
  feature,
  status,
  onDelete,
  onToggle,
}: {
  feature: RouteFeature;
  status: FeatureStatus;
  onDelete: () => void;
  onToggle: (enabled: boolean) => void;
}) {
  const configStr = JSON.stringify(feature.config);
  const inert = status.inert.length > 0;
  return (
    // min-w-0 the whole way down, or truncate does nothing: a flex item defaults
    // to min-width:auto and refuses to shrink below its content, so one long
    // unbreakable string (a bcrypt hash is exactly that) widens the row, the tab,
    // and the dialog with it.
    <div className="w-full min-w-0 space-y-1.5 rounded border p-2 text-sm">
      <div className="flex min-w-0 items-center justify-between gap-2">
        <div className="flex min-w-0 flex-1 items-center gap-2">
          {/* A Switch, not a checkbox: flipping this saves the feature
              immediately — there is no submit button on the row.

              An inert feature reads OFF and cannot be flipped. Showing ON for
              something nothing applies is the failure this whole row exists to
              prevent: a domain that looks rate-limited and is not. */}
          <Switch
            className="shrink-0"
            checked={feature.enabled && !inert}
            disabled={inert}
            onCheckedChange={(enabled) => onToggle(enabled)}
            aria-label={`Enable ${feature.feature_type}`}
          />
          <Badge variant="outline" className="shrink-0">
            {feature.feature_type}
          </Badge>
          {inert && (
            <Badge className="shrink-0 border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400">
              Not active
            </Badge>
          )}
          <span className="text-muted-foreground min-w-0 flex-1 truncate font-mono text-xs">
            {configStr}
          </span>
        </div>
        <Tooltip>
          <TooltipTrigger
            render={
              <Button
                size="icon-sm"
                variant="ghost"
                // aria-label, not just the tooltip: an icon-only button is unnamed
                // to a screen reader, and the tooltip text is not a substitute.
                aria-label="Remove feature"
                className="text-destructive hover:bg-destructive/10 hover:text-destructive shrink-0"
                onClick={onDelete}
              />
            }
          >
            <Trash2Icon className="h-4 w-4" />
          </TooltipTrigger>
          <TooltipPositioner>
            <TooltipContent>Remove Feature</TooltipContent>
          </TooltipPositioner>
        </Tooltip>
      </div>
      {/* One line per reason, kept apart: a legacy row on a Caddy that has the
          module needs "reconfigure this", not "your Caddy lacks the module". */}
      {status.inert.map((reason) => (
        <p
          key={reason.kind}
          className="text-muted-foreground text-xs"
          data-testid={`feature-inert-${reason.kind}`}
        >
          {inertExplanation(reason)}
        </p>
      ))}
    </div>
  );
}

/**
 * Route features in their own dialog, not a tab on the edit form.
 *
 * They do not belong to that form: every change here is an immediate upsert or
 * delete against its own endpoint, so sitting beside a "Save Changes" button
 * they read as though they save with it — and they do not. Their own dialog says
 * what is true.
 */
export function DomainFeaturesDialog({
  projectId,
  applicationId,
  domain,
  open,
  onOpenChange,
}: {
  projectId: string;
  applicationId: string;
  domain?: DomainExpanded;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      {/* grid-cols-[minmax(0,1fr)]: DialogContent is a grid whose implicit column
          is max-content, so one long unbreakable string (a bcrypt hash) would
          widen the column itself and no min-w-0/truncate on the children could
          shrink it. */}
      <DialogContent className="max-h-[calc(100dvh-2rem)] grid-cols-[minmax(0,1fr)] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Route Features</DialogTitle>
          <DialogDescription>
            Middleware Caddy applies to{" "}
            <span className="font-mono">{domain?.hostname}</span>. Changes take
            effect immediately.
          </DialogDescription>
        </DialogHeader>
        {domain && (
          <DomainFeatures
            key={domain.id}
            projectId={projectId}
            applicationId={applicationId}
            domain={domain}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}
