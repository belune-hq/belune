import { useForm } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { HeartPulse } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { fieldError } from "@/lib/utils/field-error";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "@/components/ui/segmented-control";
import { useSetHealthCheck } from "@/lib/hooks/use-applications";
import { useDomains } from "@/lib/hooks/use-domains";
import { resolvePorts } from "@/lib/utils/ports";
import type { Application } from "@/lib/types";

/**
 * Configures how the application's health is checked. Three mechanisms:
 *
 *  - None — no check.
 *  - HTTP — the control plane probes a path once after each deploy. Simple, and
 *    needs nothing installed in the image, but only works for HTTP services.
 *  - Command — a native Docker HEALTHCHECK run inside the container, continuously.
 *    Works for anything (a database, a queue, a worker), and because it keeps
 *    running, the container's health drives the application's status: a failing
 *    check shows the app as Unhealthy, not Running.
 *
 * A change takes effect on the next deploy (or reload) — the check is part of
 * the container, so the running one keeps its old check until then.
 */
type HealthType = "none" | "http" | "command";

// A blank numeric input means "use the default"; it is sent as undefined so the
// server stores NULL and the platform default applies.
function numOrUndefined(s: string): number | undefined {
  const n = parseInt(s, 10);
  return Number.isFinite(n) && n > 0 ? n : undefined;
}

const positiveNumber = z
  .string()
  .refine(
    (v) => v === "" || (/^\d+$/.test(v) && Number(v) > 0),
    "Must be a positive whole number",
  );

// Blank clears the port, so it is valid; anything else must be a real port.
const portRule = z.string().refine((v) => {
  if (v === "") return true;
  const n = Number(v);
  return Number.isInteger(n) && n >= 1 && n <= 65535;
}, "Port must be between 1 and 65535");

export function HealthCheckSection({
  projectId,
  applicationId,
  application,
}: {
  projectId: string;
  applicationId: string;
  application: Application;
}) {
  const setHealthCheck = useSetHealthCheck(projectId, applicationId);
  const { data: domains } = useDomains(projectId, applicationId);
  const resolved = resolvePorts(application, domains ?? []);

  const form = useForm({
    defaultValues: {
      port: application.container_port?.toString() ?? "",
      type: application.health_check_type as HealthType,
      path: application.health_check_path ?? "",
      expectStatus: application.health_check_expect_status?.toString() ?? "",
      command: application.health_check_command ?? "",
      interval: application.health_check_interval_seconds?.toString() ?? "",
      retries: application.health_check_retries?.toString() ?? "",
      startPeriod:
        application.health_check_start_period_seconds?.toString() ?? "",
      timeout: application.health_check_timeout_seconds?.toString() ?? "",
    },
    onSubmit: ({ value }) => {
      // Not part of the check (templates set it on apps with no HTTP check, and
      // it is the routing fallback regardless), but it rides on this request.
      // The endpoint reads blank-as-absent as "leave it", so a clear is spelled 0.
      const container_port = value.port === "" ? 0 : Number(value.port);
      const data =
        value.type === "none"
          ? { type: "none" as const, container_port }
          : value.type === "http"
            ? {
                type: "http" as const,
                container_port,
                path: value.path.trim(),
                expect_status: numOrUndefined(value.expectStatus),
                timeout_seconds: numOrUndefined(value.timeout),
              }
            : {
                type: "command" as const,
                container_port,
                command: value.command.trim(),
                interval_seconds: numOrUndefined(value.interval),
                retries: numOrUndefined(value.retries),
                start_period_seconds: numOrUndefined(value.startPeriod),
                timeout_seconds: numOrUndefined(value.timeout),
              };

      toast.promise(setHealthCheck.mutateAsync(data), {
        loading: "Saving...",
        success: "Health check saved — applies on the next deploy",
        error: (err) => err.message,
      });
    },
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <HeartPulse aria-hidden="true" className="size-4" />
          Health Check
        </CardTitle>
        <CardDescription>
          How the platform decides the application is healthy. A command check
          runs continuously inside the container and marks the app Unhealthy
          when it fails; an HTTP check is probed once after each deploy. Changes
          apply on the next deploy.
        </CardDescription>
      </CardHeader>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          e.stopPropagation();
          form.handleSubmit();
        }}
      >
        <CardContent className="space-y-4">
          <form.Field
            name="type"
            children={(field) => (
              <div className="space-y-2">
                <Label>Method</Label>
                <SegmentedControl
                  value={field.state.value}
                  onValueChange={(v) => field.handleChange(v as HealthType)}
                >
                  <SegmentedControlItem value="none">None</SegmentedControlItem>
                  <SegmentedControlItem value="http">HTTP</SegmentedControlItem>
                  <SegmentedControlItem value="command">
                    Command
                  </SegmentedControlItem>
                </SegmentedControl>
              </div>
            )}
          />

          <form.Subscribe
            selector={(s) => s.values.type}
            children={(type) => (
              <>
                {type === "http" && (
                  <div className="space-y-4">
                    <form.Field
                      name="path"
                      // No onChangeListenTo on "type": it ran this validator as the
                      // field unmounted on a method switch, parking an error that then
                      // rendered the moment the field came back — red on a field nobody
                      // had touched. Save is gated on the values themselves (canSave
                      // below), so nothing needed the cross-field run.
                      validators={{
                        onChange: ({ value, fieldApi }) =>
                          fieldApi.form.state.values.type === "http" &&
                          value.trim() === ""
                            ? "Path is required"
                            : undefined,
                      }}
                      children={(field) => {
                        // Visited-only, matching certificates.tsx: an untouched
                        // field never shows a message or an invalid border.
                        const error = field.state.meta.isTouched
                          ? fieldError(field.state.meta.errors)
                          : undefined;
                        return (
                          <Field data-invalid={!!error}>
                            <FieldLabel>Path</FieldLabel>
                            <Input
                              value={field.state.value}
                              onBlur={field.handleBlur}
                              onChange={(e) =>
                                field.handleChange(e.target.value)
                              }
                              placeholder="/healthz"
                              className="font-mono"
                              aria-invalid={!!error}
                            />
                            {error ? (
                              <FieldError>{error}</FieldError>
                            ) : (
                              <p className="text-muted-foreground text-xs">
                                Probed on the container's port after each
                                deploy. A non-2xx response (or the code below)
                                fails the deploy.
                              </p>
                            )}
                          </Field>
                        );
                      }}
                    />
                    <div className="grid grid-cols-2 gap-4">
                      <form.Field
                        name="expectStatus"
                        children={(field) => (
                          <div className="space-y-2">
                            <Label>Expected status</Label>
                            <Input
                              type="number"
                              value={field.state.value}
                              onBlur={field.handleBlur}
                              onChange={(e) =>
                                field.handleChange(e.target.value)
                              }
                              placeholder="any 2xx"
                            />
                          </div>
                        )}
                      />
                      <form.Field
                        name="timeout"
                        validators={{ onChange: positiveNumber }}
                        children={(field) => {
                          const error = fieldError(field.state.meta.errors);
                          return (
                            <Field data-invalid={!!error}>
                              <FieldLabel>Timeout (seconds)</FieldLabel>
                              <Input
                                type="number"
                                value={field.state.value}
                                onBlur={field.handleBlur}
                                onChange={(e) =>
                                  field.handleChange(e.target.value)
                                }
                                placeholder="120"
                                aria-invalid={!!error}
                              />
                              {error && <FieldError>{error}</FieldError>}
                            </Field>
                          );
                        }}
                      />
                    </div>
                  </div>
                )}

                {type === "command" && (
                  <div className="space-y-4">
                    <form.Field
                      name="command"
                      // No onChangeListenTo on "type": it ran this validator as the
                      // field unmounted on a method switch, parking an error that then
                      // rendered the moment the field came back — red on a field nobody
                      // had touched. Save is gated on the values themselves (canSave
                      // below), so nothing needed the cross-field run.
                      validators={{
                        onChange: ({ value, fieldApi }) =>
                          fieldApi.form.state.values.type === "command" &&
                          value.trim() === ""
                            ? "Command is required"
                            : undefined,
                      }}
                      children={(field) => {
                        const error = field.state.meta.isTouched
                          ? fieldError(field.state.meta.errors)
                          : undefined;
                        return (
                          <Field data-invalid={!!error}>
                            <FieldLabel>Command</FieldLabel>
                            <Input
                              value={field.state.value}
                              onBlur={field.handleBlur}
                              onChange={(e) =>
                                field.handleChange(e.target.value)
                              }
                              placeholder="curl -f http://localhost:3000/health || exit 1"
                              className="font-mono"
                              aria-invalid={!!error}
                            />
                            {error ? (
                              <FieldError>{error}</FieldError>
                            ) : (
                              <p className="text-muted-foreground text-xs">
                                Run inside the container via <code>sh -c</code>.
                                Exit 0 = healthy. The tool you use (curl, wget,
                                pg_isready…) must exist in the image.
                              </p>
                            )}
                          </Field>
                        );
                      }}
                    />
                    <div className="grid grid-cols-2 gap-4">
                      <form.Field
                        name="interval"
                        validators={{ onChange: positiveNumber }}
                        children={(field) => {
                          const error = fieldError(field.state.meta.errors);
                          return (
                            <Field data-invalid={!!error}>
                              <FieldLabel>Interval (seconds)</FieldLabel>
                              <Input
                                type="number"
                                value={field.state.value}
                                onBlur={field.handleBlur}
                                onChange={(e) =>
                                  field.handleChange(e.target.value)
                                }
                                placeholder="30"
                                aria-invalid={!!error}
                              />
                              {error && <FieldError>{error}</FieldError>}
                            </Field>
                          );
                        }}
                      />
                      <form.Field
                        name="timeout"
                        validators={{ onChange: positiveNumber }}
                        children={(field) => {
                          const error = fieldError(field.state.meta.errors);
                          return (
                            <Field data-invalid={!!error}>
                              <FieldLabel>Timeout (seconds)</FieldLabel>
                              <Input
                                type="number"
                                value={field.state.value}
                                onBlur={field.handleBlur}
                                onChange={(e) =>
                                  field.handleChange(e.target.value)
                                }
                                placeholder="30"
                                aria-invalid={!!error}
                              />
                              {error && <FieldError>{error}</FieldError>}
                            </Field>
                          );
                        }}
                      />
                      <form.Field
                        name="retries"
                        validators={{ onChange: positiveNumber }}
                        children={(field) => {
                          const error = fieldError(field.state.meta.errors);
                          return (
                            <Field data-invalid={!!error}>
                              <FieldLabel>Retries</FieldLabel>
                              <Input
                                type="number"
                                value={field.state.value}
                                onBlur={field.handleBlur}
                                onChange={(e) =>
                                  field.handleChange(e.target.value)
                                }
                                placeholder="3"
                                aria-invalid={!!error}
                              />
                              {error ? (
                                <FieldError>{error}</FieldError>
                              ) : (
                                <p className="text-muted-foreground text-xs">
                                  Consecutive failures before Unhealthy.
                                </p>
                              )}
                            </Field>
                          );
                        }}
                      />
                      <form.Field
                        name="startPeriod"
                        validators={{ onChange: positiveNumber }}
                        children={(field) => {
                          const error = fieldError(field.state.meta.errors);
                          return (
                            <Field data-invalid={!!error}>
                              <FieldLabel>Start period (seconds)</FieldLabel>
                              <Input
                                type="number"
                                value={field.state.value}
                                onBlur={field.handleBlur}
                                onChange={(e) =>
                                  field.handleChange(e.target.value)
                                }
                                placeholder="0"
                                aria-invalid={!!error}
                              />
                              {error ? (
                                <FieldError>{error}</FieldError>
                              ) : (
                                <p className="text-muted-foreground text-xs">
                                  Grace window at startup where failures don't
                                  count.
                                </p>
                              )}
                            </Field>
                          );
                        }}
                      />
                    </div>
                  </div>
                )}
              </>
            )}
          />

          {/* Card-level, not part of the check: the port is also the proxy's
              upstream fallback, and templates set it on apps whose method is
              none — so it stays visible whichever method is selected. */}
          <Separator />

          <form.Field
            name="port"
            validators={{ onChange: portRule }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error}>
                  <FieldLabel htmlFor="container-port">
                    Container Port
                  </FieldLabel>
                  <Input
                    id="container-port"
                    type="number"
                    min="1"
                    max="65535"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    placeholder="8080"
                    aria-invalid={!!error}
                  />
                  {error ? (
                    <FieldError>{error}</FieldError>
                  ) : (
                    <p className="text-muted-foreground text-xs">
                      The port your application listens on inside its container
                      — what an HTTP health check probes, and what a domain
                      routes to unless it sets a port of its own. Leave blank
                      for 8080.
                    </p>
                  )}
                  {resolved.healthPort !== null && (
                    <p className="text-muted-foreground text-xs">
                      Health checks probe{" "}
                      <span className="font-mono">:{resolved.healthPort}</span>{" "}
                      {resolved.healthSource === "app"
                        ? "(this port)"
                        : resolved.healthSource === "domain"
                          ? "(from the newest domain that sets one)"
                          : "(the default)"}
                    </p>
                  )}
                </Field>
              );
            }}
          />

          <div className="flex justify-end">
            <form.Subscribe
              selector={(s) =>
                [s.values.type, s.values.path, s.values.command] as const
              }
              children={([type, path, command]) => {
                const canSave =
                  type === "none" ||
                  (type === "http" && path.trim() !== "") ||
                  (type === "command" && command.trim() !== "");
                return (
                  <Button
                    type="submit"
                    disabled={!canSave || setHealthCheck.isPending}
                  >
                    {setHealthCheck.isPending ? "Saving..." : "Save"}
                  </Button>
                );
              }}
            />
          </div>
        </CardContent>
      </form>
    </Card>
  );
}
