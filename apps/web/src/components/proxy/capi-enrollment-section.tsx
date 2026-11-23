import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { toast } from "sonner";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@/components/ui/dialog";
import { Field, FieldGroup, FieldError } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import type { CAPIStatus } from "@/lib/api/capi";
import { useEnrollCAPI, useCheckCAPIEnrollment } from "@/lib/hooks/use-capi";

export function CAPIEnrollmentSection({
  status,
  busy,
  onBusyChange,
  onRestart,
}: {
  status: CAPIStatus;
  busy: boolean;
  onBusyChange: (busy: boolean) => void;
  onRestart: () => Promise<void>;
}) {
  const enroll = useEnrollCAPI();
  const check = useCheckCAPIEnrollment();
  const [mode, setMode] = useState<"quick" | "key" | null>(null);
  const ready =
    status.state === "enabled" &&
    status.health === "healthy" &&
    status.sharing_available;
  const disabled = busy || enroll.isPending || check.isPending || !ready;
  const record = status.enrollment ?? enroll.data ?? check.data;
  const form = useForm({
    defaultValues: { key: "", consent: false, overwrite: false },
    onSubmit: async ({ value }) => {
      if (!mode || !value.consent || (mode === "key" && !value.key.trim()))
        return;
      onBusyChange(true);
      try {
        await enroll.mutateAsync({
          mode,
          key: mode === "key" ? value.key : undefined,
          overwrite: mode === "key" && value.overwrite,
          consent: value.consent,
          sharing: status.sharing,
        });
        form.reset();
        setMode(null);
      } catch (error) {
        toast.error(
          error instanceof Error
            ? error.message
            : "Could not request console enrollment",
        );
      } finally {
        form.setFieldValue("key", "");
        onBusyChange(false);
      }
    },
  });
  const open = (next: "quick" | "key") => {
    form.reset();
    setMode(next);
  };
  const checkEnrollment = async () => {
    onBusyChange(true);
    try {
      await check.mutateAsync();
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : "Could not check enrollment",
      );
    } finally {
      onBusyChange(false);
    }
  };
  return (
    <section
      aria-labelledby="capi-console-heading"
      className="flex flex-col gap-4"
    >
      <Separator />
      <div className="flex flex-wrap items-center gap-3">
        <h3 id="capi-console-heading" className="font-medium">
          CrowdSec console account
        </h3>
        <Badge variant="secondary">
          {record?.state === "enrolled"
            ? "Enrolled"
            : record?.state === "awaiting_approval"
              ? "Awaiting approval"
              : "Enrollment not confirmed"}
        </Badge>
      </div>
      <p className="text-muted-foreground text-sm">
        Link this instance and its alerts to your CrowdSec account. This is
        separate from anonymous community protection.
      </p>
      {!ready && (
        <p className="text-muted-foreground text-sm">
          Enable Central API and wait for a healthy local API before enrolling
          or checking enrollment.
        </p>
      )}
      {record?.reason && (
        <Alert>
          <AlertTitle>
            {record.outcome === "already_enrolled"
              ? "Already enrolled"
              : record.outcome === "link_unreadable"
                ? "Approval link unavailable"
                : "Console enrollment"}
          </AlertTitle>
          <AlertDescription>{record.reason}</AlertDescription>
        </Alert>
      )}
      {record?.checked_at && (
        <p className="text-muted-foreground text-sm">
          Enrollment last checked {new Date(record.checked_at).toLocaleString()}
        </p>
      )}
      {enroll.data?.url &&
        record?.state === "awaiting_approval" &&
        record.requested_at === enroll.data.requested_at && (
          <div className="flex flex-col items-start gap-2">
            <Button
              render={
                <a
                  href={enroll.data.url}
                  target="_blank"
                  rel="noopener noreferrer"
                />
              }
            >
              Open CrowdSec approval link
            </Button>
            <p className="text-muted-foreground text-sm">
              This temporary link is shown only for this request and is not
              stored. If it expires, use an enrollment key.
            </p>
          </div>
        )}
      <div className="flex flex-wrap gap-2">
        <Button
          variant="outline"
          disabled={disabled}
          onClick={() => open("quick")}
        >
          Enroll with approval link
        </Button>
        <Button
          variant="outline"
          disabled={disabled}
          onClick={() => open("key")}
        >
          Use enrollment key
        </Button>
        <Button
          variant="outline"
          disabled={disabled}
          onClick={() => void checkEnrollment()}
        >
          {check.isPending ? "Checking enrollment…" : "Check enrollment"}
        </Button>
      </div>
      <p className="text-muted-foreground text-sm">
        Approval happens in your CrowdSec account. Checking enrollment contacts
        CrowdSec once; refreshes and page focus never check it.
      </p>
      {record?.restart_required && (
        <div className="flex flex-col items-start gap-2">
          <p className="text-muted-foreground text-sm">
            After accepting in CrowdSec, restart the local API, then check
            enrollment. Restarting briefly interrupts local protection.
          </p>
          <Button
            variant="outline"
            disabled={disabled}
            onClick={() => void onRestart()}
          >
            I accepted enrollment — restart CrowdSec
          </Button>
        </div>
      )}
      <Dialog
        open={mode !== null}
        onOpenChange={(open) => {
          if (!open) {
            setMode(null);
            form.reset();
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Link this instance to CrowdSec?</DialogTitle>
            <DialogDescription>
              Enrollment associates this server and its alerts with your
              CrowdSec console account. Alerts include source IPs and scenario
              names. When enabled below, alert context can include attacked
              usernames, and manual decisions include your own bans and their
              target addresses. Enrollment uses your saved sharing preferences
              exactly.
            </DialogDescription>
          </DialogHeader>
          <ul className="text-sm">
            <li>
              Manual decisions:{" "}
              {status.sharing.share_manual_decisions ? "On" : "Off"}
            </li>
            <li>
              Custom scenario alerts:{" "}
              {status.sharing.share_custom ? "On" : "Off"}
            </li>
            <li>
              Modified scenario alerts:{" "}
              {status.sharing.share_tainted ? "On" : "Off"}
            </li>
            <li>
              Alert context (including attacked usernames):{" "}
              {status.sharing.share_context ? "On" : "Off"}
            </li>
          </ul>
          <form
            className="flex flex-col gap-4"
            onSubmit={(event) => {
              event.preventDefault();
              event.stopPropagation();
              void form.handleSubmit();
            }}
          >
            <FieldGroup>
              {mode === "key" && (
                <>
                  <form.Field
                    name="key"
                    validators={{
                      onChange: ({ value }) =>
                        !value.trim()
                          ? "Enter an enrollment key"
                          : value.length > 4096
                            ? "Enrollment key is too long"
                            : undefined,
                    }}
                    children={(field) => (
                      <Field data-invalid={field.state.meta.errors.length > 0}>
                        <Label htmlFor="capi-enroll-key">Enrollment key</Label>
                        <Input
                          id="capi-enroll-key"
                          type="password"
                          autoComplete="new-password"
                          value={field.state.value}
                          onBlur={field.handleBlur}
                          onChange={(event) =>
                            field.handleChange(event.target.value)
                          }
                          aria-invalid={field.state.meta.errors.length > 0}
                        />
                        <FieldError>
                          {field.state.meta.errors.join(" ")}
                        </FieldError>
                      </Field>
                    )}
                  />
                  <p className="text-muted-foreground text-sm">
                    The key is used once and is not saved. Quick enrollment
                    cannot replace an existing enrollment; use the key option
                    below only when you intend to replace it.
                  </p>
                  <form.Field
                    name="overwrite"
                    children={(field) => (
                      <Field orientation="horizontal">
                        <Checkbox
                          id="capi-enroll-overwrite"
                          checked={field.state.value}
                          onCheckedChange={(value) =>
                            field.handleChange(value === true)
                          }
                        />
                        <Label htmlFor="capi-enroll-overwrite">
                          Replace the existing console enrollment
                        </Label>
                      </Field>
                    )}
                  />
                </>
              )}
              <form.Field
                name="consent"
                validators={{
                  onChange: ({ value }) =>
                    value
                      ? undefined
                      : "Confirm the sharing preferences before enrolling",
                }}
                children={(field) => (
                  <Field
                    orientation="horizontal"
                    data-invalid={field.state.meta.errors.length > 0}
                  >
                    <Checkbox
                      id="capi-enroll-consent"
                      checked={field.state.value}
                      onCheckedChange={(value) =>
                        field.handleChange(value === true)
                      }
                      aria-invalid={field.state.meta.errors.length > 0}
                    />
                    <Label htmlFor="capi-enroll-consent">
                      I agree to link this instance and share the data listed
                      above
                    </Label>
                  </Field>
                )}
              />
            </FieldGroup>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                disabled={busy}
                onClick={() => {
                  setMode(null);
                  form.reset();
                }}
              >
                Cancel
              </Button>
              <form.Subscribe
                selector={(state) => state.values}
                children={(value) => (
                  <Button
                    type="submit"
                    disabled={
                      disabled ||
                      !value.consent ||
                      (mode === "key" && !value.key.trim())
                    }
                  >
                    {enroll.isPending
                      ? "Requesting enrollment…"
                      : "Request console enrollment"}
                  </Button>
                )}
              />
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </section>
  );
}
