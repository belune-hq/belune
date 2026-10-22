import { useMemo, useState, type ReactNode } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useAuthStore } from "@/lib/stores/auth";
import { useForm } from "@tanstack/react-form";
import { z } from "zod";
import type { ColumnDef } from "@tanstack/react-table";
import { Globe, Lock, Trash2 } from "lucide-react";
import { RouteError } from "@/lib/components/route-error";
import {
  useCertificates,
  useUploadCertificate,
  useDeleteCertificate,
  useDomainTLSStatus,
} from "@/lib/hooks/use-certificates";
import type { Certificate, DomainTLSStatus } from "@/lib/api/certificates";
import { DNSProvidersCard } from "@/components/dns-providers/dns-providers-card";
import { ExpiryCell } from "@/components/certificates/expiry-cell";
import { EXPIRY_WARNING_DAYS, daysUntil } from "@/lib/expiry";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { PageHeader } from "@/components/ui/page-header";
import { Badge } from "@/components/ui/badge";
import { Field, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { DataTable, buildActionColumnDef } from "@/components/ui/data-table";
import {
  Tooltip,
  TooltipContent,
  TooltipPositioner,
  TooltipTrigger,
} from "@/components/ui/tooltip";
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

export const Route = createFileRoute("/_app/certificates")({
  component: CertificatesPage,
  errorComponent: RouteError,
});

// EXPIRY_WARNING_DAYS mirrors the threshold the backend will notify on.
// How many subject badges a row shows before deferring to the detail panel.
// Enough to identify the certificate at a glance; few enough that a 40-SAN cert
// cannot turn one row into half a page.
const SUBJECTS_SHOWN = 3;

// This page has two halves with two different audiences, which is why the split
// is by CONTENT rather than by locking the whole route.
//
// The certificate store stays admin-only: /api/certificates (list/upload/delete)
// carries RequireRole("admin"), the store is install-wide by design, created_by
// is provenance rather than ownership, and `subjects` is a hostname array — so a
// full listing would hand any member the domain names of every other
// application on the instance.
//
// The Domain TLS table is not admin data and no longer behaves as if it were:
// /api/domains/tls filters on the caller (OR p.shared), so a member sees the
// state of their own projects' domains and nothing else. Locking them out of it
// left them with no at-a-glance view — a member could read the certificate
// serving ONE domain from that domain's TLS badge, and had no way to see which
// of their domains was failing without opening each application in turn, which
// is the very thing this view exists to prevent.
//
// The split wraps the content instead of living inside it: the hooks in
// CertificatesContent fetch on mount, and a hook cannot be called conditionally.
function CertificatesPage() {
  const isAdmin = useAuthStore((s) => s.user?.role === "admin");

  if (!isAdmin) {
    return (
      <div className="space-y-6">
        <PageHeader
          icon={<Lock className="size-5" />}
          title="Certificates"
          description="The TLS state of your domains, and which certificate each one serves."
        />
        <p className="text-muted-foreground text-sm">
          Covers domains in your own projects and any shared with you. Uploading
          and removing certificates is an administrator action.
        </p>
        <DomainTLSTable />
      </div>
    );
  }

  return <CertificatesContent />;
}

function CertificatesContent() {
  const { data: certificates, isLoading } = useCertificates();
  const [uploadOpen, setUploadOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Certificate | null>(null);

  const expiringSoon = useMemo(
    () =>
      (certificates ?? []).filter((c) => {
        const days = daysUntil(c.not_after);
        return days !== null && days <= EXPIRY_WARNING_DAYS;
      }).length,
    [certificates],
  );

  const columns = useMemo<ColumnDef<Certificate>[]>(
    () => [
      {
        accessorKey: "name",
        header: "Name",
        cell: ({ row }) => (
          <span className="font-medium">{row.original.name}</span>
        ),
      },
      {
        accessorKey: "subjects",
        header: "Subjects",
        // Capped, because nothing bounded this: a certificate with 40 SANs — an
        // ordinary internal or wildcard cert — grew its row to several hundred
        // pixels and pushed the rest of the page off the screen. The full list
        // lives in the detail panel, which is what the panel is for.
        cell: ({ row }) => {
          const subjects = row.original.subjects;
          const shown = subjects.slice(0, SUBJECTS_SHOWN);
          const rest = subjects.length - shown.length;
          return (
            <div className="flex flex-wrap items-center gap-1">
              {shown.map((s) => (
                <Badge
                  key={s}
                  variant="secondary"
                  className="font-mono text-xs"
                >
                  {s}
                </Badge>
              ))}
              {rest > 0 && (
                <span className="text-muted-foreground text-xs">
                  +{rest} more
                </span>
              )}
            </div>
          );
        },
      },
      {
        accessorKey: "not_after",
        header: "Expires",
        cell: ({ row }) => <ExpiryCell notAfter={row.original.not_after} />,
      },
      {
        accessorKey: "domain_count",
        header: "In use",
        cell: ({ row }) => {
          const count = row.original.domain_count;
          return count === 0 ? (
            <span className="text-muted-foreground">Unused</span>
          ) : (
            <span>
              {count} {count === 1 ? "domain" : "domains"}
            </span>
          );
        },
      },
      buildActionColumnDef<Certificate>({
        meta: { headerClassName: "text-right", className: "text-right" },
        cell: ({ row: { original: cert } }) => (
          <div className="flex justify-end">
            <Tooltip>
              <TooltipTrigger
                render={
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label="Delete certificate"
                    className="text-destructive hover:bg-destructive/10 hover:text-destructive"
                    onClick={() => setDeleteTarget(cert)}
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
    <div className="space-y-6">
      <PageHeader
        icon={<Lock className="size-5" />}
        title={
          <>
            Certificates
            {certificates && certificates.length > 0 && (
              <span className="text-muted-foreground ml-2 text-base font-normal">
                {certificates.length}{" "}
                {certificates.length === 1 ? "certificate" : "certificates"}
                {expiringSoon > 0 && (
                  <span className="text-status-building">
                    {" "}
                    · {expiringSoon} expiring soon
                  </span>
                )}
              </span>
            )}
          </>
        }
        description="Certificates you upload once and select on any domain set to Custom SSL. Domains using Automatic get a certificate from Let's Encrypt instead — nothing to upload here."
      />

      <Card>
        <CardHeader className="flex flex-row items-center justify-between">
          <CardTitle className="flex items-center gap-2">
            <Lock aria-hidden="true" className="size-4" />
            HTTPS Certificates
          </CardTitle>
          <Button size="sm" onClick={() => setUploadOpen(true)}>
            Upload Certificate
          </Button>
        </CardHeader>
        <CardContent>
          <DataTable
            columns={columns}
            data={certificates ?? []}
            isLoading={isLoading}
            getRowId={(c) => c.id}
            enableSorting
            renderDetailPanel={({ row }) => (
              <CertificateDetail certificate={row.original} />
            )}
            emptyMessage='No certificates uploaded. Click "Upload Certificate" to add one.'
          />
        </CardContent>
      </Card>

      <DNSProvidersCard />

      <DomainTLSTable />

      <UploadCertificateDialog open={uploadOpen} onOpenChange={setUploadOpen} />
      {deleteTarget && (
        <DeleteCertificateDialog
          certificate={deleteTarget}
          open={true}
          onOpenChange={(open) => !open && setDeleteTarget(null)}
        />
      )}
    </div>
  );
}

const uploadSchema = z.object({
  name: z.string().min(1, "Name is required"),
  cert_pem: z.string().includes("BEGIN CERTIFICATE", {
    message: "Expected a PEM block starting with -----BEGIN CERTIFICATE-----",
  }),
  key_pem: z.string().includes("PRIVATE KEY", {
    message: "Expected a PEM block containing -----BEGIN PRIVATE KEY-----",
  }),
});

function UploadCertificateDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const upload = useUploadCertificate();

  const form = useForm({
    defaultValues: { name: "", cert_pem: "", key_pem: "" },
    validators: { onSubmit: uploadSchema },
    onSubmit: async ({ value }) => {
      await upload.mutateAsync(value);
      form.reset();
      onOpenChange(false);
    },
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      {/* The shared DialogContent sets no max height, so on a short viewport a
          tall dialog runs off both edges and the footer becomes unreachable.
          Cap it and let the body scroll. */}
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Upload Certificate</DialogTitle>
          <DialogDescription>
            Paste the full certificate chain and its private key. The key is
            encrypted at rest and never shown again.
          </DialogDescription>
        </DialogHeader>

        <form
          onSubmit={(e) => {
            e.preventDefault();
            void form.handleSubmit();
          }}
          className="space-y-4"
        >
          <form.Field name="name">
            {(field) => (
              <Field data-invalid={isFieldInvalid(field)}>
                <FieldLabel htmlFor="name">Name</FieldLabel>
                <Input
                  id="name"
                  placeholder="example.com origin"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                  onBlur={field.handleBlur}
                  aria-invalid={isFieldInvalid(field)}
                />
                <FieldError field={field} />
              </Field>
            )}
          </form.Field>

          <form.Field name="cert_pem">
            {(field) => (
              <Field data-invalid={isFieldInvalid(field)}>
                <FieldLabel htmlFor="cert_pem">Certificate (PEM)</FieldLabel>
                <Textarea
                  id="cert_pem"
                  rows={7}
                  spellCheck={false}
                  // field-sizing-fixed opts out of the shared Textarea's
                  // content-sizing, which would otherwise grow the box to fit
                  // and push a pasted chain past the bottom of the dialog. Held
                  // at rows, the textarea scrolls on its own.
                  className="field-sizing-fixed resize-y overflow-y-auto font-mono text-xs"
                  placeholder="-----BEGIN CERTIFICATE-----"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                  onBlur={field.handleBlur}
                  aria-invalid={isFieldInvalid(field)}
                />
                <p className="text-muted-foreground text-xs">
                  Include any intermediate certificates below the leaf.
                </p>
                <FieldError field={field} />
              </Field>
            )}
          </form.Field>

          <form.Field name="key_pem">
            {(field) => (
              <Field data-invalid={isFieldInvalid(field)}>
                <FieldLabel htmlFor="key_pem">Private Key (PEM)</FieldLabel>
                <Textarea
                  id="key_pem"
                  rows={7}
                  spellCheck={false}
                  className="field-sizing-fixed resize-y overflow-y-auto font-mono text-xs"
                  placeholder="-----BEGIN PRIVATE KEY-----"
                  value={field.state.value}
                  onChange={(e) => field.handleChange(e.target.value)}
                  onBlur={field.handleBlur}
                  aria-invalid={isFieldInvalid(field)}
                />
                <FieldError field={field} />
              </Field>
            )}
          </form.Field>

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={upload.isPending}>
              {upload.isPending ? "Uploading…" : "Upload"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

type TanStackFieldMeta = {
  state: { meta: { errors: unknown[]; isTouched: boolean } };
};

// True once the field both has an error and has been visited — matches
// FieldError below, so the border/label and the message agree on when to
// switch into the error state rather than the border jumping red on the
// first keystroke while the message waits for blur.
function isFieldInvalid(field: TanStackFieldMeta): boolean {
  return field.state.meta.errors.length > 0 && field.state.meta.isTouched;
}

// FieldError renders the first validation message for a TanStack form field.
function FieldError({ field }: { field: TanStackFieldMeta }) {
  const error = field.state.meta.errors[0];
  if (!error || !field.state.meta.isTouched) return null;
  const message =
    typeof error === "string"
      ? error
      : ((error as { message?: string }).message ?? "Invalid value");
  return <p className="text-destructive text-xs">{message}</p>;
}

function DeleteCertificateDialog({
  certificate,
  open,
  onOpenChange,
}: {
  certificate: Certificate;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const del = useDeleteCertificate();
  const inUse = certificate.domain_count > 0;

  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete {certificate.name}?</AlertDialogTitle>
          <AlertDialogDescription>
            {inUse
              ? `${certificate.domain_count} ${certificate.domain_count === 1 ? "domain is" : "domains are"} still serving this certificate. Point them at another certificate first — deleting it would break TLS for them.`
              : "This certificate is not in use. Deleting it cannot be undone; you would need to upload the PEM pair again."}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={inUse || del.isPending}
            onClick={async () => {
              await del.mutateAsync(certificate.id);
              onOpenChange(false);
            }}
          >
            Delete
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

// Every status wears the same shape: coloured text on a low-opacity fill of the
// same colour, with a matching border. That is what `destructive` already is
// (border-destructive/20 bg-destructive/10 text-destructive), so Failed set the
// pattern and the rest now follow it instead of being solid blocks of colour.
//
// Active uses the `light` variant, which is that same pattern in the accent —
// so it tracks whatever accent the user picked. Pending and Expiring use the
// amber status tokens, which exist for exactly this: `-soft` is the fill and
// `-line` is the border, both defined per-theme in index.css.
const TLS_STATUS_STYLES: Record<
  string,
  {
    label: string;
    variant: "default" | "secondary" | "outline" | "destructive" | "light";
    className?: string;
  }
> = {
  active: { label: "Active", variant: "light" },
  pending: {
    label: "Pending",
    variant: "outline",
    className:
      "border-status-building-line bg-status-building-soft text-status-building",
  },
  expiring: {
    label: "Expiring",
    variant: "outline",
    className:
      "border-status-building-line bg-status-building-soft text-status-building",
  },
  expired: { label: "Expired", variant: "destructive" },
  failed: { label: "Failed", variant: "destructive" },
  disabled: { label: "Off", variant: "outline" },
  // The proxy is configured to issue from its own CA (dev). A real, finished
  // state — not a step on the way to a public certificate — but the certificate
  // is trusted by nothing, so it is not dressed up as Active.
  local: { label: "Local", variant: "secondary" },
  unknown: { label: "Checking…", variant: "secondary" },
};

/**
 * Every domain's TLS state in one place. Without this an operator has to open
 * each application in turn to discover which certificate is stuck — the thing
 * that makes a silent ACME failure so expensive to notice.
 */
function DomainTLSTable() {
  const { data: domains, isLoading } = useDomainTLSStatus();

  const columns = useMemo<ColumnDef<DomainTLSStatus>[]>(
    () => [
      {
        accessorKey: "hostname",
        header: "Domain",
        cell: ({ row }) => (
          <div className="flex flex-col">
            <span className="font-medium">{row.original.hostname}</span>
            <span className="text-muted-foreground text-xs">
              {row.original.application_name}
            </span>
          </div>
        ),
      },
      {
        accessorKey: "tls_status",
        header: "Status",
        cell: ({ row }) => {
          const style =
            TLS_STATUS_STYLES[row.original.tls_status] ??
            TLS_STATUS_STYLES.unknown;
          return (
            <Badge variant={style.variant} className={style.className}>
              {style.label}
            </Badge>
          );
        },
      },
      {
        accessorKey: "ssl_mode",
        header: "Mode",
        // Two lines, mirroring the Domain column. `capitalize` sits on the mode
        // alone: it used to wrap both, which title-cased the certificate's name —
        // a name the user chose, and not ours to rewrite.
        cell: ({ row }) => (
          <div className="flex flex-col">
            <span className="text-muted-foreground capitalize">
              {row.original.ssl_mode}
            </span>
            {row.original.certificate_name && (
              <span className="text-muted-foreground text-xs">
                {row.original.certificate_name}
              </span>
            )}
          </div>
        ),
      },
      {
        accessorKey: "tls_issuer",
        header: "Issuer",
        cell: ({ row }) => (
          <span className="text-muted-foreground truncate">
            {row.original.tls_issuer || "—"}
          </span>
        ),
      },
      {
        accessorKey: "tls_not_after",
        header: "Expires",
        cell: ({ row }) => (
          <ExpiryCell
            notAfter={row.original.tls_not_after}
            quiet={row.original.tls_status === "local"}
          />
        ),
      },
    ],
    [],
  );

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Globe aria-hidden="true" className="size-4" />
          Domain TLS
        </CardTitle>
      </CardHeader>
      <CardContent>
        <DataTable
          columns={columns}
          data={domains ?? []}
          isLoading={isLoading}
          getRowId={(d) => d.id}
          enableSorting
          renderDetailPanel={({ row }) => (
            <DomainTLSDetail domain={row.original} />
          )}
          emptyMessage="No domains configured yet."
        />
      </CardContent>
    </Card>
  );
}

function DetailField({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="space-y-1">
      <p className="text-text-faint text-xs font-medium tracking-wider uppercase">
        {label}
      </p>
      <div className="text-sm break-words">{children}</div>
    </div>
  );
}

function CertificateDetail({ certificate }: { certificate: Certificate }) {
  const { subjects } = certificate;
  return (
    <div className="space-y-4 py-1">
      <div className="grid gap-4 sm:grid-cols-2">
        <DetailField label="Issuer">
          {certificate.issuer || (
            <span className="text-muted-foreground">Unknown</span>
          )}
        </DetailField>
        <DetailField label="Added">
          {new Date(certificate.created_at).toLocaleString()}
        </DetailField>
      </div>
      {subjects.length > SUBJECTS_SHOWN && (
        <DetailField label={`Subjects (${subjects.length})`}>
          <div className="flex flex-wrap gap-1">
            {subjects.map((s) => (
              <Badge key={s} variant="secondary" className="font-mono text-xs">
                {s}
              </Badge>
            ))}
          </div>
        </DetailField>
      )}
    </div>
  );
}

// The reason lives here rather than in a column: it is a paragraph, and a
// paragraph in a table cell either wraps the row to three lines or gets
// truncated to uselessness. Last checked comes along so the panel is worth
// opening on a healthy domain too, rather than expanding to "nothing to report".
function DomainTLSDetail({ domain }: { domain: DomainTLSStatus }) {
  const { tls_error: error, tls_advisory: advisory } = domain;

  return (
    <div className="space-y-4 py-1">
      <DetailField label="Detail">
        {error ? (
          <span className="text-destructive">{error}</span>
        ) : advisory ? (
          // An advisory is a suspicion, not a verdict — a hostname resolving
          // somewhere that isn't us is also just what a proxy in front of us
          // looks like. A caution, and only when nothing worse is known.
          <span className="text-status-building">{advisory}</span>
        ) : (
          <span className="text-muted-foreground">No issues reported.</span>
        )}
      </DetailField>
      <DetailField label="Last checked">
        {domain.tls_last_checked_at ? (
          new Date(domain.tls_last_checked_at).toLocaleString()
        ) : (
          <span className="text-muted-foreground">Not yet checked</span>
        )}
      </DetailField>
    </div>
  );
}
