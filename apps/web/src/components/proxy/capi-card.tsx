import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { toast } from "sonner";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldGroup } from "@/components/ui/field";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  useCAPIStatus,
  useChangeCAPI,
  useCheckCAPIConnection,
} from "@/lib/hooks/use-capi";
import type { CAPIChange, CAPISharing, CAPIStatus } from "@/lib/api/capi";

const options: {
  name: keyof CAPISharing;
  title: string;
  description: string;
}[] = [
  {
    name: "share_manual_decisions",
    title: "Share manual decisions",
    description:
      "Send manually added ban decisions, including their target addresses, to CrowdSec.",
  },
  {
    name: "share_custom",
    title: "Share custom scenario signals",
    description:
      "Send alerts from scenarios defined on this instance to CrowdSec.",
  },
  {
    name: "share_tainted",
    title: "Share modified scenario signals",
    description:
      "Send alerts from locally modified community scenarios to CrowdSec.",
  },
  {
    name: "share_context",
    title: "Share alert context",
    description:
      "Send additional context attached to alerts. It may contain request or log details.",
  },
];
const labels: Record<CAPIStatus["state"], string> = {
  not_registered: "Not registered",
  enabled: "Enabled",
  failed_reverted: "Failed and reverted",
  failed: "Change failed",
  unavailable: "Unavailable",
};

export function CAPICard() {
  const query = useCAPIStatus();
  return (
    <Card>
      <CardHeader>
        <CardTitle>Community protection (Central API)</CardTitle>
        <CardDescription>
          CrowdSec's Central API downloads community blocklists and receives
          detection signals from this instance, such as source IPs and scenario
          names. Enabling it anonymously registers this instance with CrowdSec.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {query.isLoading ? (
          <Skeleton className="h-24 w-full" />
        ) : query.isError || !query.data ? (
          <Alert variant="destructive">
            <AlertTitle>Central API status unavailable</AlertTitle>
            <AlertDescription>
              {query.error?.message ?? "Could not read Central API status."}
            </AlertDescription>
          </Alert>
        ) : (
          <CAPIForm
            key={JSON.stringify(query.data.sharing)}
            status={query.data}
          />
        )}
      </CardContent>
    </Card>
  );
}

function CAPIForm({ status }: { status: CAPIStatus }) {
  const change = useChangeCAPI();
  const connectionCheck = useCheckCAPIConnection();
  const [confirm, setConfirm] = useState(false);
  const busy = status.busy || change.isPending || connectionCheck.isPending;
  const connection = status.connection ?? connectionCheck.data;
  const checkConnection = async () => {
    try {
      await connectionCheck.mutateAsync();
    } catch (error) {
      toast.error(
        error instanceof Error
          ? error.message
          : "Could not check Central API connection",
      );
    }
  };
  const queue = async (request: CAPIChange) => {
    try {
      await change.mutateAsync(request);
      setConfirm(false);
      toast.success("Central API change queued");
    } catch (error) {
      toast.error(
        error instanceof Error
          ? error.message
          : "Could not queue Central API change",
      );
    }
  };
  const form = useForm({
    defaultValues: status.sharing,
    onSubmit: async ({ value }) => {
      await queue({ sharing: value });
    },
  });
  const known = ["enabled", "not_registered", "failed_reverted"].includes(
    status.state,
  );
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <Badge
          variant={
            status.state === "failed" || status.state === "failed_reverted"
              ? "destructive"
              : "secondary"
          }
        >
          {status.busy
            ? status.phase === "queued"
              ? "Queued"
              : "Applying…"
            : labels[status.state]}
        </Badge>
        {known && status.sharing_available && status.health === "healthy" ? (
          <div className="flex items-center gap-2">
            <Label htmlFor="capi-enabled">Central API</Label>
            <Switch
              id="capi-enabled"
              checked={status.state === "enabled"}
              disabled={
                busy || !status.sharing_available || status.health !== "healthy"
              }
              onCheckedChange={(enabled) => {
                if (enabled) setConfirm(true);
                else void queue({ enabled: false });
              }}
            />
          </div>
        ) : status.health ? (
          <Button
            variant="outline"
            disabled={busy}
            onClick={() => void queue({ enabled: false })}
          >
            Disable Central API
          </Button>
        ) : null}
      </div>
      <p className="text-muted-foreground text-sm">
        Status refreshes use local configuration and health only. Checking the
        connection contacts CrowdSec and authenticates once.
      </p>
      <Button
        variant="outline"
        className="self-start"
        disabled={busy || status.state !== "enabled"}
        onClick={() => void checkConnection()}
      >
        {connectionCheck.isPending
          ? "Checking connection…"
          : "Check connection"}
      </Button>
      {connection && (
        <Alert
          variant={connection.state === "connected" ? "default" : "destructive"}
        >
          <AlertTitle>
            {connection.state === "rate_limited"
              ? "Central API rate limit"
              : "Last connection check"}
          </AlertTitle>
          <AlertDescription>
            {connection.reason}
            {connection.checked_at && (
              <span className="block">
                Checked {new Date(connection.checked_at).toLocaleString()}
              </span>
            )}
          </AlertDescription>
        </Alert>
      )}
      {status.reason && (
        <Alert variant="destructive">
          <AlertTitle>
            {status.state === "failed_reverted"
              ? "Local protection restored"
              : "Central API needs attention"}
          </AlertTitle>
          <AlertDescription>{status.reason}</AlertDescription>
        </Alert>
      )}
      {status.busy && (
        <p role="status" className="text-muted-foreground text-sm">
          Applying the change and waiting for CrowdSec to become healthy. This
          can take a few minutes. The result appears here automatically.
        </p>
      )}
      <p className="text-muted-foreground text-sm">
        The switches below control additional data shared with CrowdSec. They
        show this instance's current preferences; custom and modified scenario
        sharing are enabled by CrowdSec on a stock install.
      </p>
      {status.sharing_available ? (
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault();
            event.stopPropagation();
            void form.handleSubmit();
          }}
        >
          <FieldGroup>
            {options.map((option) => (
              <form.Field
                key={option.name}
                name={option.name}
                children={(field) => (
                  <Field
                    orientation="horizontal"
                    data-disabled={busy || !status.sharing_available}
                  >
                    <div className="flex flex-1 flex-col gap-1">
                      <Label htmlFor={`capi-${option.name}`}>
                        {option.title}
                      </Label>
                      <FieldDescription id={`capi-${option.name}-description`}>
                        {option.description}
                      </FieldDescription>
                    </div>
                    <Switch
                      id={`capi-${option.name}`}
                      aria-describedby={`capi-${option.name}-description`}
                      checked={field.state.value}
                      disabled={busy || !status.sharing_available}
                      onCheckedChange={field.handleChange}
                    />
                  </Field>
                )}
              />
            ))}
          </FieldGroup>
          <form.Subscribe
            selector={(state) => state.isPristine}
            children={(pristine) => (
              <Button
                type="submit"
                variant="outline"
                className="self-start"
                disabled={
                  busy ||
                  !status.sharing_available ||
                  status.state !== "enabled" ||
                  pristine
                }
              >
                Save sharing preferences
              </Button>
            )}
          />
        </form>
      ) : (
        <p className="text-muted-foreground text-sm">
          Sharing preferences could not be read from CrowdSec. Try again when
          the local API is healthy.
        </p>
      )}
      {status.sharing_available && status.state !== "enabled" && (
        <p className="text-muted-foreground text-sm">
          Your selected preferences apply when you confirm enabling Central API.
        </p>
      )}
      <Dialog open={confirm} onOpenChange={setConfirm}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Enable Central API sharing?</DialogTitle>
            <DialogDescription>
              This anonymously registers this instance with CrowdSec, downloads
              community blocklists, and sends detection signals to CrowdSec.
              Additional sharing follows the preferences listed below.
            </DialogDescription>
          </DialogHeader>
          <form.Subscribe
            selector={(state) => state.values}
            children={(values) => (
              <ul className="text-sm">
                {options.map((option) => (
                  <li key={option.name}>
                    {option.title}: {values[option.name] ? "On" : "Off"}
                  </li>
                ))}
              </ul>
            )}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirm(false)}>
              Cancel
            </Button>
            <Button
              disabled={busy}
              onClick={() =>
                void queue({ enabled: true, sharing: form.state.values })
              }
            >
              Enable and share signals
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
