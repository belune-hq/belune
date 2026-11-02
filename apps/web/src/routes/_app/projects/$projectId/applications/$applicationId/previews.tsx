import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { Loader2 } from "lucide-react";

import { useApplication } from "@/lib/hooks/use-applications";
import {
  usePreviews,
  useUpdatePreviewConfig,
  useDeletePreview,
} from "@/lib/hooks/use-previews";
import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { fieldError } from "@/lib/utils/field-error";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusBadge } from "@/lib/components/status-badge";
import { formatDateTimeShort } from "@/lib/utils/format";
import { RouteError } from "@/lib/components/route-error";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId/previews",
)({
  component: PreviewsPage,
  errorComponent: RouteError,
});

function PreviewsPage() {
  const { projectId, applicationId } = Route.useParams();
  const { data: application, isLoading: applicationLoading } = useApplication(
    projectId,
    applicationId,
  );
  const { data: previewsData, isLoading } = usePreviews(
    projectId,
    applicationId,
  );
  const updateConfig = useUpdatePreviewConfig(projectId, applicationId);
  const deletePreview = useDeletePreview(projectId, applicationId);

  if (application?.parent_application_id) {
    return (
      <div className="text-muted-foreground text-sm">
        This is a preview environment; it cannot host nested previews.
      </div>
    );
  }

  return (
    <div className="space-y-6">
      {applicationLoading ? (
        <Card>
          <CardContent className="space-y-4 pt-6">
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
          </CardContent>
        </Card>
      ) : (
        <ConfigCard
          // Remounts once the save/disable mutation's invalidation lands, so
          // the form's defaultValues re-derive from the fresh application
          // record instead of racing useForm's own defaultValues-sync effect.
          // Not mounted until the application has loaded — keying on "" here
          // first and remounting once real data arrives caused the same
          // double-render InstanceNameField/ServerIpField hit on server.tsx.
          key={`${application?.preview_branch_pattern ?? ""}-${application?.preview_domain_template ?? ""}`}
          application={application}
          onSave={async (pattern, template) => {
            await toast.promise(
              updateConfig.mutateAsync({
                preview_branch_pattern: pattern,
                preview_domain_template: template,
              }),
              {
                loading: "Saving...",
                success: "Preview config saved",
                error: (err) => err.message,
              },
            );
          }}
        />
      )}

      <Card>
        <CardHeader>
          <CardTitle>Active Preview Environments</CardTitle>
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <div className="space-y-2">
              {[1, 2].map((i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : !previewsData?.previews.length ? (
            <p className="text-muted-foreground text-sm">
              No preview environments yet. Push a branch matching the pattern to
              spin one up.
            </p>
          ) : (
            <div
              role="list"
              aria-label="Preview environments"
              className="divide-y text-sm"
            >
              {previewsData.previews.map((preview) => {
                const branchLabel = preview.branch || "unknown";
                return (
                  <div
                    key={preview.id}
                    role="listitem"
                    aria-label={`Preview environment for branch ${branchLabel}`}
                    className="flex items-center justify-between gap-4 py-3"
                  >
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <span className="truncate font-medium">
                          {branchLabel}
                        </span>
                        <StatusBadge status={preview.status} />
                      </div>
                      {preview.hostname && preview.url && (
                        <a
                          href={preview.url}
                          target="_blank"
                          rel="noopener noreferrer"
                          aria-label={`Open ${branchLabel} preview at ${preview.hostname} in a new tab`}
                          className="text-muted-foreground truncate font-mono text-xs hover:underline"
                        >
                          {preview.hostname}
                        </a>
                      )}
                      <div className="text-muted-foreground text-xs">
                        Last activity:{" "}
                        {formatDateTimeShort(preview.last_activity_at)}
                      </div>
                    </div>
                    <AlertDialog>
                      <AlertDialogTrigger
                        render={
                          <Button
                            size="sm"
                            variant="outline"
                            aria-label={`Delete preview for branch ${branchLabel}`}
                          />
                        }
                      >
                        Delete
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogHeader>
                          <AlertDialogTitle>Delete preview?</AlertDialogTitle>
                          <AlertDialogDescription>
                            This removes the preview container and all its
                            resources. A subsequent push to{" "}
                            <span className="font-mono">{preview.branch}</span>{" "}
                            will recreate it.
                          </AlertDialogDescription>
                        </AlertDialogHeader>
                        <AlertDialogFooter>
                          <AlertDialogCancel>Cancel</AlertDialogCancel>
                          <AlertDialogAction
                            onClick={() => {
                              toast.promise(
                                deletePreview.mutateAsync(preview.id),
                                {
                                  loading: "Deleting...",
                                  success: "Preview deleted",
                                  error: (err) => err.message,
                                },
                              );
                            }}
                          >
                            Delete
                          </AlertDialogAction>
                        </AlertDialogFooter>
                      </AlertDialogContent>
                    </AlertDialog>
                  </div>
                );
              })}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function ConfigCard({
  application,
  onSave,
}: {
  application: ReturnType<typeof useApplication>["data"];
  onSave: (pattern: string, template: string) => Promise<void>;
}) {
  const [saving, setSaving] = useState(false);

  const enabled = !!(
    application?.preview_branch_pattern && application?.preview_domain_template
  );

  const form = useForm({
    defaultValues: {
      pattern: application?.preview_branch_pattern ?? "",
      template: application?.preview_domain_template ?? "",
    },
    onSubmit: async ({ value }) => {
      setSaving(true);
      try {
        await onSave(value.pattern, value.template);
      } finally {
        setSaving(false);
      }
    },
  });

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center gap-2">
          <CardTitle>Preview Configuration</CardTitle>
          <Badge variant={enabled ? "default" : "outline"}>
            {enabled ? "Enabled" : "Disabled"}
          </Badge>
        </div>
      </CardHeader>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          e.stopPropagation();
          form.handleSubmit();
        }}
      >
        <CardContent className="space-y-4">
          <p className="text-muted-foreground text-sm">
            When a push arrives on a branch matching the pattern (and not the
            auto-deploy branch), a dedicated preview environment is built and
            exposed at a templated hostname.
          </p>
          <form.Field
            name="pattern"
            children={(field) => (
              <div className="space-y-2">
                <Label htmlFor="preview-pattern">Branch pattern</Label>
                <Input
                  id="preview-pattern"
                  placeholder="feature/*"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                />
                <p className="text-muted-foreground text-xs">
                  Glob-style. Use <span className="font-mono">*</span> for a
                  single segment or <span className="font-mono">**</span> for
                  many (e.g. <span className="font-mono">feature/**</span>).
                </p>
              </div>
            )}
          />
          <form.Field
            name="template"
            validators={{
              onChange: z
                .string()
                .refine(
                  (v) => !v || v.includes("{branch}"),
                  "Template must contain {branch}",
                ),
            }}
            children={(field) => {
              const error = fieldError(field.state.meta.errors);
              return (
                <Field data-invalid={!!error}>
                  <FieldLabel htmlFor="preview-template">
                    Domain template
                  </FieldLabel>
                  <Input
                    id="preview-template"
                    placeholder="{branch}.{app}.preview.example.com"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={!!error}
                  />
                  {error ? (
                    <FieldError>{error}</FieldError>
                  ) : (
                    <p className="text-muted-foreground text-xs">
                      Must contain{" "}
                      <span className="font-mono">{"{branch}"}</span>.{" "}
                      <span className="font-mono">{"{app}"}</span> expands to
                      this application's slug. A wildcard DNS record (e.g.{" "}
                      <span className="font-mono">*.preview.example.com</span>)
                      pointing to this host is required.
                    </p>
                  )}
                </Field>
              );
            }}
          />
          <form.Subscribe
            selector={(s) => [s.values.pattern, s.values.template] as const}
            children={([pattern, template]) => (
              <div className="flex justify-end gap-2">
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={async () => {
                    setSaving(true);
                    try {
                      await onSave("", "");
                    } finally {
                      setSaving(false);
                    }
                  }}
                  disabled={saving || (!pattern && !template)}
                >
                  Disable
                </Button>
                <Button type="submit" size="sm" disabled={saving}>
                  {saving ? (
                    <Loader2
                      aria-hidden="true"
                      className="h-4 w-4 animate-spin"
                    />
                  ) : (
                    "Save"
                  )}
                </Button>
              </div>
            )}
          />
        </CardContent>
      </form>
    </Card>
  );
}
