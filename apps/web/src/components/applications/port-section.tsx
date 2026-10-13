import { useForm } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { Network } from "lucide-react";
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
import { PendingChangeBadge } from "@/lib/components/pending-change-badge";
import { useSetContainerPort } from "@/lib/hooks/use-applications";
import { useDomains } from "@/lib/hooks/use-domains";
import { resolvePorts } from "@/lib/utils/ports";
import type { Application } from "@/lib/types";

// Blank clears the port, so it is valid; anything else must be a real port.
const portRule = z.string().refine((v) => {
  if (v === "") return true;
  const n = Number(v);
  return Number.isInteger(n) && n >= 1 && n <= 65535;
}, "Port must be between 1 and 65535");

/**
 * The port the application's container listens on, split out of the general
 * settings form (which rewrites every column) like the Resources card. Saved,
 * not applied: the health check URL and the proxy route both take the port at
 * deploy time, so saving stamps the config marker and the header badge points
 * at Reload.
 *
 * Below the field is what the SAVED configuration resolves to, because the
 * health check and the routes use opposite precedence (the check prefers this
 * port, a route prefers its own domain's) and that is easier to read than to
 * explain. A domain's port differing from this one is the legitimate
 * multi-port case, so it is shown plainly and never warned about.
 */
export function PortSection({
  projectId,
  applicationId,
  application,
}: {
  projectId: string;
  applicationId: string;
  application: Application;
}) {
  const setPort = useSetContainerPort(projectId, applicationId);
  const { data: domains } = useDomains(projectId, applicationId);
  const resolved = resolvePorts(application, domains ?? []);

  const form = useForm({
    defaultValues: {
      port: application.container_port?.toString() ?? "",
    },
    onSubmit: ({ value }) => {
      toast.promise(
        setPort.mutateAsync({
          // The endpoint requires the field; 0 is how a clear is spelled.
          container_port: value.port === "" ? 0 : Number(value.port),
        }),
        {
          loading: "Saving...",
          success: "Container port saved",
          error: (err) => err.message,
        },
      );
    },
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Network aria-hidden="true" className="size-4" />
          Port
        </CardTitle>
        <CardDescription>
          The port your application listens on inside its container. Applied on
          the next deploy.
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
                    placeholder="Not set"
                    aria-invalid={!!error}
                  />
                  {error ? (
                    <FieldError>{error}</FieldError>
                  ) : (
                    <p className="text-muted-foreground text-xs">
                      Leave blank to use a domain's port, then 8080. A domain
                      can still route to a different port of its own.
                    </p>
                  )}
                </Field>
              );
            }}
          />

          <dl
            className="bg-muted/40 space-y-1.5 rounded-md p-3 text-sm"
            aria-label="Resolved ports"
          >
            <div className="flex flex-wrap gap-x-2">
              <dt className="text-muted-foreground">Health checks probe</dt>
              {/* The gap is CSS; this keeps copied and read-out text spaced. */}{" "}
              <dd>
                {resolved.healthPort === null ? (
                  <span className="text-muted-foreground">
                    nothing over HTTP (no HTTP health check is configured)
                  </span>
                ) : (
                  <>
                    <span className="font-mono">:{resolved.healthPort}</span>{" "}
                    <span className="text-muted-foreground text-xs">
                      {resolved.healthSource === "app"
                        ? "(this port)"
                        : resolved.healthSource === "domain"
                          ? "(from the newest domain that sets one)"
                          : "(the default)"}
                    </span>
                  </>
                )}
              </dd>
            </div>
            {resolved.routes.length > 0 && (
              <div className="flex flex-wrap gap-x-2">
                <dt className="text-muted-foreground">Routing</dt>{" "}
                <dd className="space-y-0.5">
                  {resolved.routes.map((r, i) => (
                    <div key={`${r.hostname}${r.path}${i}`}>
                      <span>
                        {r.hostname}
                        {r.path !== "/" ? r.path : ""}
                      </span>{" "}
                      <span className="text-muted-foreground">→</span>{" "}
                      <span className="font-mono">:{r.port}</span>
                      {r.overridden && (
                        <span className="text-muted-foreground text-xs">
                          {" "}
                          (set on the domain)
                        </span>
                      )}
                    </div>
                  ))}
                </dd>
              </div>
            )}
          </dl>

          <div className="flex items-center justify-end gap-3">
            <PendingChangeBadge app={application} className="mr-auto" />
            <Button type="submit" disabled={setPort.isPending}>
              {setPort.isPending ? "Saving..." : "Save"}
            </Button>
          </div>
        </CardContent>
      </form>
    </Card>
  );
}
