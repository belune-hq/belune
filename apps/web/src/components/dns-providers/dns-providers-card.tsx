import { useMemo, useState } from "react";
import { useForm, useStore } from "@tanstack/react-form";
import { z } from "zod";
import type { ColumnDef } from "@tanstack/react-table";
import { GlobeLock, KeyRound, Trash2 } from "lucide-react";
import {
  useCreateDNSProvider,
  useDeleteDNSProvider,
  useDNSProviders,
  useUpdateDNSProvider,
} from "@/lib/hooks/use-dns-providers";
import type { DNSProvider, DNSProviderType } from "@/lib/api/dns-providers";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { DataTable, buildActionColumnDef } from "@/components/ui/data-table";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  Tooltip,
  TooltipContent,
  TooltipPositioner,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { fieldError } from "@/lib/utils/field-error";

interface CredentialField {
  key: string;
  label: string;
  required: boolean;
  /** Rendered as a password input and never echoed back. */
  secret: boolean;
  help?: string;
}

// Mirrors proxy.DNSProviders on the server, whose keys are the Caddy modules'
// own JSON names. The server is the authority: it refuses an unknown key, so a
// drift here shows as a 400 with the reason rather than as a stored credential
// that can never authenticate.
const DNS_PROVIDERS: Record<
  DNSProviderType,
  { label: string; fields: CredentialField[] }
> = {
  cloudflare: {
    label: "Cloudflare",
    fields: [
      {
        key: "api_token",
        label: "API token",
        required: true,
        secret: true,
        help: "Needs Zone → DNS → Edit on the zones you will issue for.",
      },
      {
        key: "zone_token",
        label: "Zone token",
        required: false,
        secret: true,
        help: "Optional Zone → Zone → Read token, for an API token scoped to a single zone.",
      },
    ],
  },
  digitalocean: {
    label: "DigitalOcean",
    fields: [
      { key: "auth_token", label: "API token", required: true, secret: true },
    ],
  },
  route53: {
    label: "AWS Route 53",
    fields: [
      {
        key: "access_key_id",
        label: "Access key ID",
        required: true,
        secret: false,
      },
      {
        key: "secret_access_key",
        label: "Secret access key",
        required: true,
        secret: true,
      },
      {
        key: "session_token",
        label: "Session token",
        required: false,
        secret: true,
      },
      { key: "region", label: "Region", required: false, secret: false },
      {
        key: "hosted_zone_id",
        label: "Hosted zone ID",
        required: false,
        secret: false,
        help: "Only needed when the same domain exists in more than one hosted zone.",
      },
    ],
  },
};

// Shown once the field has been left or a submit was attempted. isTouched flips
// on the first keystroke and would paint a field red while it is still being
// typed; isBlurred alone would hide a never-visited required field after a
// failed submit.
function showError(
  field: { state: { meta: { errors: unknown[]; isBlurred: boolean } } },
  submitted: boolean,
) {
  return field.state.meta.isBlurred || submitted
    ? fieldError(field.state.meta.errors)
    : undefined;
}

export function DNSProvidersCard() {
  const { data: providers, isLoading } = useDNSProviders();
  const [editing, setEditing] = useState<DNSProvider | "new" | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<DNSProvider | null>(null);

  const columns = useMemo<ColumnDef<DNSProvider>[]>(
    () => [
      {
        accessorKey: "name",
        header: "Name",
        cell: ({ row }) => (
          <span className="font-medium">{row.original.name}</span>
        ),
      },
      {
        accessorKey: "provider",
        header: "Provider",
        cell: ({ row }) => DNS_PROVIDERS[row.original.provider].label,
      },
      {
        accessorKey: "domain_count",
        header: "In use",
        cell: ({ row }) => {
          const n = row.original.domain_count;
          return n === 0 ? (
            <span className="text-muted-foreground">Unused</span>
          ) : (
            <span>
              {n} {n === 1 ? "domain" : "domains"}
            </span>
          );
        },
      },
      buildActionColumnDef<DNSProvider>({
        meta: { headerClassName: "text-right", className: "text-right" },
        cell: ({ row: { original: p } }) => (
          <div className="flex justify-end gap-1">
            <Tooltip>
              <TooltipTrigger
                render={
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={`Edit ${p.name}`}
                    onClick={() => setEditing(p)}
                  />
                }
              >
                <KeyRound className="h-4 w-4" />
              </TooltipTrigger>
              <TooltipPositioner>
                <TooltipContent>Rename or rotate credentials</TooltipContent>
              </TooltipPositioner>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger
                render={
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={`Delete ${p.name}`}
                    className="text-destructive hover:bg-destructive/10 hover:text-destructive"
                    onClick={() => setDeleteTarget(p)}
                  />
                }
              >
                <Trash2 className="h-4 w-4" />
              </TooltipTrigger>
              <TooltipPositioner>
                <TooltipContent>Delete</TooltipContent>
              </TooltipPositioner>
            </Tooltip>
          </div>
        ),
      }),
    ],
    [],
  );

  return (
    <>
      <Card>
        <CardHeader className="flex flex-row items-center justify-between">
          <CardTitle className="flex items-center gap-2">
            <GlobeLock aria-hidden="true" className="size-4" />
            DNS Providers
          </CardTitle>
          <Button size="sm" onClick={() => setEditing("new")}>
            Add Provider
          </Button>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-muted-foreground text-sm">
            Credentials for the DNS challenge, which issues certificates by
            editing a DNS record instead of answering over HTTP. It is the only
            way to get a wildcard certificate, and the way to get one for a
            domain behind Cloudflare&apos;s proxy. Needs the Belune Caddy image.
            Tokens are encrypted at rest and never shown again.
          </p>
          <DataTable
            columns={columns}
            data={providers ?? []}
            isLoading={isLoading}
            getRowId={(p) => p.id}
            emptyMessage='No DNS providers. Click "Add Provider" to add one.'
          />
        </CardContent>
      </Card>

      {editing && (
        <ProviderDialog
          // Keyed so each open starts from a clean form.
          key={editing === "new" ? "new" : editing.id}
          provider={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
        />
      )}
      {deleteTarget && (
        <DeleteProviderDialog
          provider={deleteTarget}
          onClose={() => setDeleteTarget(null)}
        />
      )}
    </>
  );
}

function ProviderDialog({
  provider,
  onClose,
}: {
  provider: DNSProvider | null;
  onClose: () => void;
}) {
  const isEdit = provider !== null;
  const create = useCreateDNSProvider();
  const update = useUpdateDNSProvider();

  const form = useForm({
    defaultValues: {
      name: provider?.name ?? "",
      provider: (provider?.provider ?? "cloudflare") as DNSProviderType,
      // One map so the credential inputs follow the selected provider.
      credentials: {} as Record<string, string>,
    },
    onSubmit: async ({ value }) => {
      const spec = DNS_PROVIDERS[value.provider];
      // Only fields that belong to the provider and were filled in. On edit a
      // wholly blank set means "keep the stored secret"; any value replaces the
      // whole set, which is why an edit that fills one field must fill the
      // required ones too (checked per field below).
      const creds = Object.fromEntries(
        spec.fields
          .map((f) => [f.key, (value.credentials[f.key] ?? "").trim()] as const)
          .filter(([, v]) => v !== ""),
      );
      const hasCreds = Object.keys(creds).length > 0;
      if (isEdit) {
        await update.mutateAsync({
          id: provider.id,
          name: value.name.trim(),
          credentials: hasCreds ? creds : undefined,
        });
      } else {
        await create.mutateAsync({
          name: value.name.trim(),
          provider: value.provider,
          credentials: creds,
        });
      }
      onClose();
    },
  });

  const pending = create.isPending || update.isPending;
  const submitted = useStore(form.store, (s) => s.submissionAttempts > 0);

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {isEdit ? `Edit ${provider.name}` : "Add DNS Provider"}
          </DialogTitle>
          <DialogDescription>
            {isEdit
              ? "Leave the credentials blank to keep the stored ones. Entering any replaces the whole set."
              : "The credential needs permission to edit DNS records in the zones you will issue for."}
          </DialogDescription>
        </DialogHeader>

        <form
          onSubmit={(e) => {
            e.preventDefault();
            void form.handleSubmit();
          }}
          className="space-y-4"
        >
          <form.Field
            name="name"
            validators={{
              onChange: z.string().trim().min(1, "Name is required"),
            }}
            children={(field) => {
              const error = showError(field, submitted);
              return (
                <Field data-invalid={!!error}>
                  <FieldLabel htmlFor="dns-name">Name</FieldLabel>
                  <Input
                    id="dns-name"
                    placeholder="Cloudflare — example.com"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={!!error}
                  />
                  {error && <FieldError>{error}</FieldError>}
                </Field>
              );
            }}
          />

          <form.Field
            name="provider"
            children={(field) => (
              <div className="space-y-2">
                <Label>Provider</Label>
                <Select
                  value={field.state.value}
                  // Fixed after creation: domains reference the row, and one
                  // provider's token fed to another's module can only fail.
                  disabled={isEdit}
                  onValueChange={(v) => {
                    if (!v) return;
                    field.handleChange(v as DNSProviderType);
                    form.setFieldValue("credentials", {});
                  }}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {(Object.keys(DNS_PROVIDERS) as DNSProviderType[]).map(
                      (k) => (
                        <SelectItem key={k} value={k}>
                          {DNS_PROVIDERS[k].label}
                        </SelectItem>
                      ),
                    )}
                  </SelectContent>
                </Select>
              </div>
            )}
          />

          <form.Subscribe
            selector={(s) => s.values.provider}
            children={(type) =>
              DNS_PROVIDERS[type].fields.map((spec) => (
                <form.Field
                  key={`${type}-${spec.key}`}
                  name={`credentials.${spec.key}`}
                  validators={{
                    // Required on create. On edit, required only once the
                    // operator starts entering a replacement set.
                    onChange: ({ value, fieldApi }) => {
                      if (!spec.required || value) return undefined;
                      const others = fieldApi.form.getFieldValue("credentials");
                      const touchedSet = Object.values(others ?? {}).some(
                        (v) => (v ?? "").trim() !== "",
                      );
                      return !isEdit || touchedSet
                        ? `${spec.label} is required`
                        : undefined;
                    },
                    onChangeListenTo: ["credentials"],
                  }}
                  children={(field) => {
                    const error = showError(field, submitted);
                    return (
                      <Field data-invalid={!!error}>
                        <FieldLabel htmlFor={`dns-${spec.key}`}>
                          {spec.label}
                          {!spec.required && (
                            <span className="text-muted-foreground ml-1 font-normal">
                              (optional)
                            </span>
                          )}
                        </FieldLabel>
                        <Input
                          id={`dns-${spec.key}`}
                          type={spec.secret ? "password" : "text"}
                          autoComplete="off"
                          spellCheck={false}
                          placeholder={
                            isEdit && spec.secret ? "Unchanged" : undefined
                          }
                          value={field.state.value ?? ""}
                          onBlur={field.handleBlur}
                          onChange={(e) => field.handleChange(e.target.value)}
                          aria-invalid={!!error}
                        />
                        {spec.help && (
                          <p className="text-muted-foreground text-xs">
                            {spec.help}
                          </p>
                        )}
                        {error && <FieldError>{error}</FieldError>}
                      </Field>
                    );
                  }}
                />
              ))
            }
          />

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={pending}>
              {pending ? "Saving…" : isEdit ? "Save" : "Add Provider"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function DeleteProviderDialog({
  provider,
  onClose,
}: {
  provider: DNSProvider;
  onClose: () => void;
}) {
  const del = useDeleteDNSProvider();
  const inUse = provider.domain_count > 0;

  return (
    <AlertDialog open onOpenChange={(open) => !open && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete {provider.name}?</AlertDialogTitle>
          <AlertDialogDescription>
            {inUse
              ? `${provider.domain_count} ${provider.domain_count === 1 ? "domain uses" : "domains use"} this provider. Switch them to another provider or SSL mode first — deleting it would leave them unable to renew.`
              : "This provider is not in use. Deleting it cannot be undone; you would need to enter the credentials again."}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={inUse || del.isPending}
            onClick={async () => {
              await del.mutateAsync(provider.id);
              onClose();
            }}
          >
            Delete
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
