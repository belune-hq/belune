import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { TriangleAlertIcon } from "lucide-react";
import { toast } from "sonner";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldError } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "@/components/ui/segmented-control";
import { Textarea } from "@/components/ui/textarea";
import {
  draftFor,
  inputFor,
  validateDraft,
  type ExclusionDraft,
} from "@/components/proxy/waf-exclusion-view";
import { eventHost } from "@/components/proxy/waf-view";
import { useCreateWafExclusion, useWafExclusions } from "@/lib/hooks/use-waf";
import { fieldError } from "@/lib/utils/field-error";
import type { WafEvent, WafExclusionShape } from "@/lib/types";

/**
 * Tell the rule set to stop looking at what a detection found.
 *
 * It is a security downgrade, and the dialog is built around saying so: the
 * warning is always on screen, the operator never types a field name (it comes from
 * the detection, and is offered only when it could be written safely), a narrow
 * scope is the default, and turning a rule off needs a reason that outlives them.
 */
export function WafExclusionDialog({
  event,
  open,
  onOpenChange,
}: {
  event: WafEvent;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { data: page } = useWafExclusions();
  // Until the list loads there is no way to know which fields may be named, so the
  // target is not offered rather than offered on a guess.
  const start = draftFor(event, page?.collections ?? []);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Create Exclusion</DialogTitle>
          <DialogDescription>
            Rule {event.rule_id}
            {event.message ? ` · ${event.message}` : ""}
          </DialogDescription>
        </DialogHeader>
        {"refusal" in start ? (
          <Alert>
            <TriangleAlertIcon aria-hidden="true" />
            <AlertTitle>This rule cannot be excluded</AlertTitle>
            <AlertDescription>{start.refusal}</AlertDescription>
          </Alert>
        ) : (
          <ExclusionForm
            event={event}
            initial={start.draft}
            targetReason={start.target.available ? null : start.target.reason}
            onDone={() => onOpenChange(false)}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

const SHAPE_HELP: Record<WafExclusionShape, string> = {
  target:
    "Ignore this one field for this rule. Everything else about the request is still checked.",
  path: "Ignore this rule for requests to this path. Use it when the rule matched the request as a whole.",
  rule: "Turn this rule off. It is the widest exclusion there is.",
};

function ExclusionForm({
  event,
  initial,
  targetReason,
  onDone,
}: {
  event: WafEvent;
  initial: ExclusionDraft;
  targetReason: string | null;
  onDone: () => void;
}) {
  const create = useCreateWafExclusion();
  // Whether a submit has been tried, held here and not read off the form: the form's
  // own counter does not re-render the subscribers that show the messages.
  const [attempted, setAttempted] = useState(false);

  const form = useForm({
    defaultValues: initial,
    onSubmit: async ({ value }) => {
      // No field validators here, so this is the gate: a bad draft stays on screen
      // with its message.
      setAttempted(true);
      if (validateDraft(event.rule_id, value) !== undefined) return;
      const saved = create.mutateAsync(inputFor(event, value));
      toast.promise(saved, {
        loading: "Creating the exclusion…",
        success: "Exclusion created",
        error: (e: Error) => e.message || "Could not create the exclusion",
      });
      await saved.then(onDone).catch(() => undefined);
    },
  });

  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault();
        e.stopPropagation();
        form.handleSubmit();
      }}
    >
      <Alert variant="warning">
        <TriangleAlertIcon aria-hidden="true" />
        <AlertTitle>This lowers your protection</AlertTitle>
        <AlertDescription>
          The WAF will stop recording this rule for what you choose below,
          whether a request is harmless or an attack. The field shown is read
          from the request itself and can be influenced by it, so check it is
          the one you mean.
        </AlertDescription>
      </Alert>

      <form.Subscribe
        selector={(s) => s.values}
        children={(v) => (
          <>
            <section className="space-y-2">
              <Label>What to ignore</Label>
              <SegmentedControl
                size="sm"
                fullWidth
                value={v.shape}
                onValueChange={(s) =>
                  form.setFieldValue("shape", s as WafExclusionShape)
                }
              >
                <SegmentedControlItem
                  value="target"
                  disabled={targetReason !== null}
                >
                  Field
                </SegmentedControlItem>
                <SegmentedControlItem value="path">Path</SegmentedControlItem>
                <SegmentedControlItem value="rule">
                  Whole rule
                </SegmentedControlItem>
              </SegmentedControl>
              {targetReason && (
                <p className="text-muted-foreground text-xs">{targetReason}</p>
              )}
              <p className="text-muted-foreground text-xs">
                {SHAPE_HELP[v.shape]}
              </p>

              {v.shape === "target" && (
                <p className="bg-muted rounded-md px-2.5 py-2 font-mono text-xs break-all">
                  {v.collection}:{v.targetName}
                </p>
              )}
              {v.shape === "path" && (
                <form.Field
                  name="pathPattern"
                  children={(field) => {
                    const error = attempted
                      ? validateDraft(event.rule_id, v)
                      : undefined;
                    return (
                      <Field data-invalid={!!error}>
                        <Label htmlFor="exclusion-path">Path</Label>
                        <Input
                          id="exclusion-path"
                          className="font-mono text-xs"
                          autoComplete="off"
                          spellCheck={false}
                          placeholder="/.env.local or /api/*"
                          value={field.state.value}
                          aria-invalid={!!error}
                          onBlur={field.handleBlur}
                          onChange={(e) => field.handleChange(e.target.value)}
                        />
                        <p className="text-muted-foreground text-xs">
                          Exact, or ending in <code>*</code> for &ldquo;begins
                          with&rdquo;.
                        </p>
                        {error && <FieldError>{error}</FieldError>}
                      </Field>
                    );
                  }}
                />
              )}
            </section>

            <section className="space-y-2">
              <Label>Where</Label>
              <SegmentedControl
                size="sm"
                fullWidth
                value={v.scope}
                onValueChange={(s) =>
                  form.setFieldValue("scope", s as ExclusionDraft["scope"])
                }
              >
                <SegmentedControlItem
                  value="application"
                  disabled={!event.application_id}
                >
                  This application
                </SegmentedControlItem>
                <SegmentedControlItem value="install">
                  Every application
                </SegmentedControlItem>
              </SegmentedControl>
              {v.scope === "install" ? (
                <p className="text-status-building text-xs">
                  Applies to every application on this server, including ones
                  you add later.
                </p>
              ) : (
                <p className="text-muted-foreground text-xs">
                  Applies to {eventHost(event)} and the application&apos;s other
                  domains, as they are now.
                </p>
              )}
              {!event.application_id && (
                <p className="text-muted-foreground text-xs">
                  This detection is not tied to an application, so it can only
                  apply to every one.
                </p>
              )}
            </section>

            <form.Field
              name="note"
              children={(field) => {
                const needed = v.shape === "rule";
                const error =
                  attempted && needed
                    ? validateDraft(event.rule_id, v)
                    : undefined;
                return (
                  <Field data-invalid={!!error}>
                    <Label htmlFor="exclusion-note">
                      {needed ? "Why" : "Why (optional)"}
                    </Label>
                    <Textarea
                      id="exclusion-note"
                      className="min-h-16 text-sm"
                      maxLength={500}
                      placeholder={
                        needed
                          ? "Required. This is kept with the exclusion and in the audit log."
                          : "Kept with the exclusion and in the audit log."
                      }
                      value={field.state.value}
                      aria-invalid={!!error}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                    {error && <FieldError>{fieldError([error])}</FieldError>}
                  </Field>
                );
              }}
            />

            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={onDone}
                disabled={create.isPending}
              >
                Cancel
              </Button>
              <Button
                type="submit"
                disabled={
                  create.isPending ||
                  (attempted && validateDraft(event.rule_id, v) !== undefined)
                }
              >
                Create Exclusion
              </Button>
            </DialogFooter>
          </>
        )}
      />
    </form>
  );
}
