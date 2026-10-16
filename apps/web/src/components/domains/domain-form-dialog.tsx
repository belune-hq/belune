import { useForm, useStore } from "@tanstack/react-form";
import { z } from "zod";
import { toast } from "sonner";
import {
  forwardedPath,
  normalizeInternalPath,
  normalizePublicPath,
  samplePublicRequest,
} from "@/lib/domain-path";
import {
  GlobeIcon,
  KeyRoundIcon,
  Loader2,
  ShieldCheckIcon,
  ShieldOffIcon,
} from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { fieldError } from "@/lib/utils/field-error";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useAddDomain, useUpdateDomain } from "@/lib/hooks/use-domains";
import {
  useCertificates,
  useUsableCertificates,
} from "@/lib/hooks/use-certificates";
import { useAuthStore } from "@/lib/stores/auth";
import type { DomainExpanded } from "@/lib/types";

const HOSTNAME_REGEX =
  /^([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$/;

// DNS Challenge is deliberately absent: it needs a Caddy build carrying DNS
// provider modules, which the stock image does not have, so offering it would
// only ever produce a domain stuck on "pending". The API rejects it too. A domain
// created before this was removed still renders (see legacyModeFor) so it can be
// switched to something that works.
const SSL_MODES = [
  { value: "automatic", label: "Automatic (ACME)", Icon: ShieldCheckIcon },
  { value: "custom", label: "Custom Certificate", Icon: KeyRoundIcon },
  { value: "off", label: "Off", Icon: ShieldOffIcon },
] as const;

/** An unsupported mode a domain is already on, so the Select is never blank. */
function legacyModeFor(mode: string | undefined) {
  if (!mode || SSL_MODES.some((m) => m.value === mode)) return null;
  return {
    value: mode,
    label: `${mode.replace(/_/g, " ")} (no longer supported)`,
    Icon: GlobeIcon,
  } as const;
}

interface Props {
  projectId: string;
  applicationId: string;
  domain?: DomainExpanded;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function DomainFormDialog({
  projectId,
  applicationId,
  domain,
  open,
  onOpenChange,
}: Props) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      {/* Capped: the shared DialogContent sets no max height, so a tall dialog (a
          long feature list) runs off both edges with the footer unreachable. */}
      {/* grid-cols-[minmax(0,1fr)]: DialogContent is a grid, and its implicit
          column is `auto` — i.e. max-content — so a child with one long
          unbreakable string (a bcrypt hash) makes the *column* that wide and no
          amount of min-w-0 or truncate on the child can stop it. Pinning the
          column to minmax(0,1fr) is what lets the children shrink at all. */}
      <DialogContent className="max-h-[calc(100dvh-2rem)] grid-cols-[minmax(0,1fr)] overflow-y-auto sm:max-w-lg">
        {/* Keyed so the form re-initializes from the current domain each open. */}
        <DomainForm
          key={`${domain?.id ?? "new"}-${open}`}
          projectId={projectId}
          applicationId={applicationId}
          domain={domain}
          onClose={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  );
}

function DomainForm({
  projectId,
  applicationId,
  domain,
  onClose,
}: {
  projectId: string;
  applicationId: string;
  domain?: DomainExpanded;
  onClose: () => void;
}) {
  const isEdit = !!domain;
  const addDomain = useAddDomain(projectId, applicationId);
  const updateDomain = useUpdateDomain(projectId, applicationId);
  const pending = addDomain.isPending || updateDomain.isPending;

  // Two different questions, because the API answers two different ones. An
  // admin lists the whole store (with every SAN, which the dropdown shows).
  // A member cannot list it at all and instead asks "what can serve THIS
  // hostname" — enough to attach one, without learning the other hosts each
  // certificate covers. Both hooks are called unconditionally and gated by
  // `enabled`, since a hook cannot be called conditionally.
  const isAdmin = useAuthStore((s) => s.user?.role === "admin");
  const legacyMode = legacyModeFor(domain?.ssl_mode);

  const form = useForm({
    defaultValues: {
      hostname: domain?.hostname ?? "",
      // Empty, not "/": a pre-filled slash means the operator who types "/api"
      // ends up with "//api". The placeholder shows the default instead, and a
      // blank field is normalised to "/" on submit.
      path: domain?.path && domain.path !== "/" ? domain.path : "",
      strip_path: domain?.strip_path ?? false,
      internal_path: domain?.internal_path ?? "",
      ssl_mode: domain?.ssl_mode ?? "automatic",
      container_port: domain?.container_port?.toString() ?? "",
      force_https: domain?.force_https ?? true,
      certificate_id: domain?.certificate_id ?? "",
    },
    onSubmit: ({ value }) => {
      const trimmed = value.hostname.trim();
      const portNum = value.container_port
        ? parseInt(value.container_port, 10)
        : null;

      // The server normalises the path (roots it, drops a trailing slash), so a
      // blank field means the whole host rather than an error.
      const path = value.path.trim() || "/";
      // Stripping "/" is meaningless — there is nothing to strip — and would only
      // travel to the server to be ignored.
      const stripPath = path === "/" ? false : value.strip_path;
      // Empty is meaningful here: it means prepend nothing. Unlike the public
      // path, it must not be coerced to "/", which would prepend a bare slash.
      const internalPath = normalizeInternalPath(value.internal_path);

      const action =
        isEdit && domain
          ? updateDomain.mutateAsync({
              domainId: domain.id,
              hostname: trimmed,
              path,
              strip_path: stripPath,
              internal_path: internalPath,
              ssl_mode: value.ssl_mode,
              force_https: value.force_https,
              // 0, not null, to clear the override. The server reads an absent
              // container_port as "keep what is stored" so that a PUT about
              // something else cannot wipe it, and JSON null is
              // indistinguishable from absent once it is decoded into a
              // pointer. 0 is the documented way to remove an override.
              container_port: portNum ?? 0,
              certificate_id: value.certificate_id || undefined,
            })
          : addDomain.mutateAsync({
              hostname: trimmed,
              path,
              strip_path: stripPath,
              internal_path: internalPath,
              ssl_enabled: value.ssl_mode !== "off",
              ssl_mode: value.ssl_mode,
              force_https: value.force_https,
              container_port: portNum ?? undefined,
              certificate_id: value.certificate_id || undefined,
            });

      toast.promise(
        action.then(() => onClose()),
        {
          loading: isEdit ? "Saving..." : "Adding...",
          success: isEdit ? "Domain updated" : "Domain added",
          error: (err) => err.message,
        },
      );
    },
  });

  // Read from the form store rather than a Subscribe, because these feed hooks
  // and a hook cannot live inside a render prop. The hostname lives on the
  // Routing tab and the picker on the TLS tab, so by the time the picker is
  // visible this value is settled — no debounce needed.
  const hostnameValue = useStore(form.store, (s) => s.values.hostname);

  const { data: adminCertificates, isLoading: adminCertsLoading } =
    useCertificates(isAdmin);
  const { data: usableCertificates, isLoading: usableCertsLoading } =
    useUsableCertificates(hostnameValue, !isAdmin);

  // One shape for the dropdown. subjects is undefined for a member: the API
  // withholds it, so the dropdown simply omits the SAN line for them rather
  // than inventing one.
  const certificates: { id: string; name: string; subjects?: string[] }[] =
    isAdmin ? (adminCertificates ?? []) : (usableCertificates ?? []);
  const certificatesLoading = isAdmin ? adminCertsLoading : usableCertsLoading;

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        e.stopPropagation();
        form.handleSubmit();
      }}
      className="space-y-4"
    >
      <DialogHeader>
        <DialogTitle>{isEdit ? "Edit Domain" : "Add Domain"}</DialogTitle>
        <DialogDescription>
          {isEdit
            ? "Update hostname, TLS and routing settings."
            : "Configure a new domain with TLS and routing settings."}
        </DialogDescription>
      </DialogHeader>

      {/* Two tabs, both honestly named. This tab used to be called "Hostname"
          while holding the hostname, the path, the strip toggle, the internal
          path and the port — the whole journey of a request from the browser to
          the container. Meanwhile "Routing" held one checkbox that was really a
          TLS decision. The labels were inverted. */}
      <Tabs defaultValue="routing" className="min-w-0">
        <TabsList className="grid w-full grid-cols-2">
          <TabsTrigger value="routing">Routing</TabsTrigger>
          <TabsTrigger value="tls">TLS</TabsTrigger>
        </TabsList>

        <TabsContent value="routing" className="space-y-3 pt-3">
          <form.Field
            name="hostname"
            validators={{
              onChange: z
                .string()
                .min(1, "Hostname is required")
                .regex(HOSTNAME_REGEX, "Invalid hostname format"),
            }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error}>
                  <FieldLabel htmlFor="hostname">Hostname</FieldLabel>
                  <Input
                    id="hostname"
                    placeholder="app.example.com"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={!!error}
                  />
                  {error ? (
                    <FieldError>{error}</FieldError>
                  ) : (
                    <p className="text-muted-foreground text-xs">
                      Fully qualified domain. Must resolve to this server.
                    </p>
                  )}
                </Field>
              );
            }}
          />
          <form.Field
            name="path"
            validators={{
              onChange: z
                .string()
                .refine(
                  (v) => !v.trim() || /^\/?[A-Za-z0-9\-._~/]*$/.test(v.trim()),
                  "Only letters, digits and - . _ ~ / — no wildcards",
                )
                .refine(
                  (v) => !v.includes("*"),
                  "No wildcard: /api already covers everything beneath it",
                ),
            }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error}>
                  <FieldLabel htmlFor="path">Path</FieldLabel>
                  <Input
                    id="path"
                    placeholder="/"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={!!error}
                  />
                  {error ? (
                    <FieldError>{error}</FieldError>
                  ) : (
                    <p className="text-muted-foreground text-xs">
                      The prefix this app answers on. Leave blank to serve the
                      whole hostname, or use /api to share one hostname between
                      several apps.
                    </p>
                  )}
                </Field>
              );
            }}
          />
          <form.Subscribe
            selector={(state) => state.values.path}
            children={(path) =>
              path.trim() && path.trim() !== "/" ? (
                <form.Field
                  name="strip_path"
                  children={(field) => (
                    <Label className="flex items-center gap-2 text-sm font-normal">
                      <Checkbox
                        checked={field.state.value}
                        onCheckedChange={(v) => field.handleChange(v)}
                      />
                      Strip the prefix before forwarding
                    </Label>
                  )}
                />
              ) : null
            }
          />
          <form.Field
            name="internal_path"
            validators={{
              onChange: z
                .string()
                .refine(
                  (v) => !v.trim() || /^\/?[A-Za-z0-9\-._~/]*$/.test(v.trim()),
                  "Only letters, digits and - . _ ~ / — no wildcards",
                ),
            }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error}>
                  <FieldLabel htmlFor="internal_path">Internal path</FieldLabel>
                  <Input
                    id="internal_path"
                    placeholder="(none)"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={!!error}
                  />
                  {error ? (
                    <FieldError>{error}</FieldError>
                  ) : (
                    <p className="text-muted-foreground text-xs">
                      Prepended before the request reaches the app. Only needed
                      when the app insists on serving under a base path of its
                      own — Grafana under /grafana. Leave blank otherwise.
                    </p>
                  )}
                </Field>
              );
            }}
          />
          {/* The three fields above are each simple and jointly incomprehensible:
              you can reason about "strip /api" or "prepend /app" alone, but not
              about what a request looks like by the time it lands. So show it. */}
          <form.Subscribe
            selector={(state) => ({
              hostname: state.values.hostname,
              path: state.values.path,
              strip: state.values.strip_path,
              internal: state.values.internal_path,
            })}
            children={({ hostname, path, strip, internal }) => {
              const publicPath = normalizePublicPath(path);
              const internalPath = normalizeInternalPath(internal);
              const stripPath = publicPath === "/" ? false : strip;
              const request = samplePublicRequest(publicPath);
              const arrives = forwardedPath(
                request,
                publicPath,
                stripPath,
                internalPath,
              );
              const host = hostname.trim() || "your-domain.com";

              return (
                <div className="bg-muted/40 space-y-2 rounded-md border p-3">
                  <p className="text-xs font-medium">For example</p>
                  <dl className="space-y-1.5 text-xs">
                    <div className="flex gap-2">
                      <dt className="text-muted-foreground w-32 shrink-0">
                        Visitor requests
                      </dt>
                      <dd className="min-w-0 font-mono break-all">
                        {host}
                        <span className="text-foreground">{request}</span>
                      </dd>
                    </div>
                    <div className="flex gap-2">
                      <dt className="text-muted-foreground w-32 shrink-0">
                        Your app receives
                      </dt>
                      <dd className="text-foreground min-w-0 font-mono break-all">
                        {arrives}
                      </dd>
                    </div>
                  </dl>
                  {stripPath || internalPath ? (
                    <p className="text-muted-foreground text-xs">
                      {stripPath ? `${publicPath} removed` : null}
                      {stripPath && internalPath ? ", then " : null}
                      {internalPath ? `${internalPath} prepended` : null}
                    </p>
                  ) : (
                    <p className="text-muted-foreground text-xs">
                      Forwarded unchanged.
                    </p>
                  )}
                </div>
              );
            }}
          />
          <form.Field
            name="container_port"
            validators={{
              onChange: z.string().refine((v) => {
                if (!v) return true;
                const n = Number(v);
                return Number.isInteger(n) && n >= 1 && n <= 65535;
              }, "Port must be between 1 and 65535"),
            }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error}>
                  <FieldLabel htmlFor="port">Container Port</FieldLabel>
                  <Input
                    id="port"
                    type="number"
                    min="1"
                    max="65535"
                    placeholder="App's port"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={!!error}
                  />
                  {error ? (
                    <FieldError>{error}</FieldError>
                  ) : (
                    <p className="text-muted-foreground text-xs">
                      The port this domain routes to inside the container. Leave
                      blank to use the application's configured port, or 8080 if
                      it has none.
                    </p>
                  )}
                </Field>
              );
            }}
          />
          <p className="text-muted-foreground text-xs">
            Route features (basic auth, custom headers, IP allowlist, redirects)
            have their own dialog — they apply immediately rather than on save.
            Find them under the domain's actions menu.
          </p>
        </TabsContent>

        <TabsContent value="tls" className="space-y-3 pt-3">
          <form.Field
            name="ssl_mode"
            children={(field) => (
              <div className="space-y-2">
                <Label>SSL Mode</Label>
                <Select
                  value={field.state.value}
                  onValueChange={(v) => field.handleChange(v ?? "automatic")}
                >
                  <SelectTrigger className="capitalize">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {[...SSL_MODES, ...(legacyMode ? [legacyMode] : [])].map(
                      (m) => (
                        <SelectItem
                          key={m.value}
                          value={m.value}
                          icon={<m.Icon />}
                          className="capitalize"
                        >
                          {m.label}
                        </SelectItem>
                      ),
                    )}
                  </SelectContent>
                </Select>
              </div>
            )}
          />
          <form.Subscribe
            selector={(s) => s.values.ssl_mode}
            children={(sslMode) =>
              sslMode === "custom" ? (
                <form.Field
                  name="certificate_id"
                  validators={{
                    onChangeListenTo: ["ssl_mode"],
                    onChange: ({ value, fieldApi }) =>
                      fieldApi.form.getFieldValue("ssl_mode") === "custom" &&
                      !value
                        ? "Select a certificate"
                        : undefined,
                  }}
                  children={(field) => {
                    const error = fieldError(field.state.meta.errors);
                    return (
                      <Field data-invalid={!!error}>
                        <FieldLabel>Certificate</FieldLabel>
                        <Select
                          value={field.state.value}
                          onValueChange={(v) => field.handleChange(v ?? "")}
                          disabled={certificatesLoading}
                        >
                          <SelectTrigger
                            className="w-full min-w-0"
                            aria-invalid={!!error}
                          >
                            {/* Name only. The default renders the whole item —
                                name *and* every subject — which a real
                                certificate (a wildcard, an Origin CA) blows the
                                dialog wide open with. The subjects stay in the
                                dropdown, where there is room for them. */}
                            <SelectValue
                              placeholder={
                                certificatesLoading
                                  ? "Loading…"
                                  : "Select a certificate"
                              }
                            >
                              {(value: unknown) => {
                                const cert = certificates.find(
                                  (c) => c.id === value,
                                );
                                if (!cert) {
                                  return certificatesLoading
                                    ? "Loading…"
                                    : "Select a certificate";
                                }
                                return (
                                  <span className="truncate">{cert.name}</span>
                                );
                              }}
                            </SelectValue>
                          </SelectTrigger>
                          <SelectContent>
                            {certificates.map((cert) => (
                              <SelectItem key={cert.id} value={cert.id}>
                                {cert.name}
                                {cert.subjects && (
                                  <span className="text-muted-foreground ml-2 font-mono text-xs">
                                    {cert.subjects.join(", ")}
                                  </span>
                                )}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                        {/* Two empty states, because "nothing here" means two
                            different things. An admin is looking at the whole
                            store, so empty really is empty. A member is looking
                            at what covers ONE hostname, so empty means nothing
                            covers it — the store may be full. Saying "no
                            certificates uploaded yet" to a member was simply
                            false, and it pointed them at a page that shows them
                            a lock screen. */}
                        {!certificatesLoading &&
                          certificates.length === 0 &&
                          (isAdmin ? (
                            <p className="text-muted-foreground text-xs">
                              No certificates uploaded yet. Add one under
                              Settings → Certificates.
                            </p>
                          ) : (
                            <p className="text-muted-foreground text-xs">
                              No certificate covers{" "}
                              {hostnameValue.trim() || "this hostname"} yet. Ask
                              an admin to upload one that does.
                            </p>
                          ))}
                        {error && <FieldError>{error}</FieldError>}
                      </Field>
                    );
                  }}
                />
              ) : null
            }
          />

          {/* Force HTTPS lives here, not under Routing: it is meaningless when
              ssl_mode=off — the route builder skips the redirect entirely — so
              offering it there let an operator tick a box that did nothing. */}
          <form.Subscribe
            selector={(state) => state.values.ssl_mode}
            children={(sslMode) => (
              <form.Field
                name="force_https"
                children={(field) => (
                  <div className="space-y-1 border-t pt-3">
                    <Label className="flex items-center gap-2 text-sm font-normal">
                      <Checkbox
                        checked={sslMode !== "off" && field.state.value}
                        disabled={sslMode === "off"}
                        onCheckedChange={(v) => field.handleChange(v)}
                      />
                      <span
                        className={
                          sslMode === "off"
                            ? "text-muted-foreground"
                            : undefined
                        }
                      >
                        Force HTTPS
                      </span>
                    </Label>
                    <p className="text-muted-foreground text-xs">
                      {sslMode === "off"
                        ? "Unavailable while TLS is off — there is no HTTPS to redirect to."
                        : "Redirect plain HTTP requests for this domain to HTTPS."}
                    </p>
                  </div>
                )}
              />
            )}
          />
        </TabsContent>
      </Tabs>

      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          onClick={onClose}
          disabled={pending}
        >
          Cancel
        </Button>
        <form.Subscribe
          selector={(s) => s.canSubmit}
          children={(canSubmit) => (
            <Button type="submit" disabled={pending || !canSubmit}>
              {pending && <Loader2 className="mr-1 h-4 w-4 animate-spin" />}
              {isEdit ? "Save Changes" : "Add Domain"}
            </Button>
          )}
        />
      </DialogFooter>
    </form>
  );
}
