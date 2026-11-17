import { copyToClipboard } from "@/lib/utils/clipboard";
import { useState } from "react";
import { useForm, useStore } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { Github, Lock, Users, Copy, Check } from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { fieldError } from "@/lib/utils/field-error";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import { useSaveGitProviderConfig } from "@/lib/hooks/use-git-providers";
import {
  getGitHubManifest,
  submitGitHubManifest,
  type GitProvider,
} from "@/lib/api/git-providers";
import { ProviderIcon } from "./provider-icon";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

const PROVIDERS: {
  value: GitProvider;
  label: string;
  selfHosted: boolean;
  baseUrlRequired: boolean;
}[] = [
  {
    value: "github",
    label: "GitHub",
    selfHosted: false,
    baseUrlRequired: false,
  },
  {
    value: "gitlab",
    label: "GitLab",
    selfHosted: true,
    baseUrlRequired: false,
  },
  {
    value: "bitbucket",
    label: "Bitbucket",
    selfHosted: false,
    baseUrlRequired: false,
  },
  { value: "gitea", label: "Gitea", selfHosted: true, baseUrlRequired: true },
];

export function ProviderFormDialog({ open, onOpenChange }: Props) {
  const save = useSaveGitProviderConfig();

  const [showSecret, setShowSecret] = useState(false);
  const [copied, setCopied] = useState(false);

  const origin = window.location.origin;
  const callbackUrl = `${origin}/api/git/integrations/callback`;

  const form = useForm({
    defaultValues: {
      provider: "github" as GitProvider,
      isPublic: false,
      org: "",
      baseUrl: "",
      clientId: "",
      clientSecret: "",
    },
    onSubmit: ({ value }) => {
      toast.promise(
        save
          .mutateAsync({
            provider: value.provider,
            base_url: value.baseUrl || undefined,
            client_id: value.clientId,
            client_secret: value.clientSecret,
            is_public: value.isPublic,
          })
          .then(close),
        {
          loading: "Saving...",
          success: "Provider added",
          error: (e) => e.message,
        },
      );
    },
  });

  const provider = useStore(form.store, (s) => s.values.provider);
  const isPublic = useStore(form.store, (s) => s.values.isPublic);
  const meta = PROVIDERS.find((p) => p.value === provider)!;
  const isGitHub = provider === "github";

  const reset = () => {
    form.reset();
    setShowSecret(false);
  };

  const close = () => {
    onOpenChange(false);
    reset();
  };

  const copyCallback = async () => {
    if (!(await copyToClipboard(callbackUrl))) {
      toast.error("Failed to copy");
      return;
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  const startGitHubApp = async () => {
    try {
      const manifest = await getGitHubManifest(
        form.state.values.org || undefined,
        isPublic,
      );
      submitGitHubManifest(manifest);
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : "Failed to start GitHub App setup",
      );
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => (o ? onOpenChange(true) : close())}
    >
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            e.stopPropagation();
            form.handleSubmit();
          }}
          className="space-y-4"
        >
          <DialogHeader>
            <DialogTitle>Add provider</DialogTitle>
            <DialogDescription>
              Register a Git OAuth app or GitHub App so team members can connect
              their accounts.
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-5">
            {/* Provider selector */}
            <form.Field
              name="provider"
              children={(field) => (
                <div className="space-y-2">
                  <Label>Provider</Label>
                  <div className="grid grid-cols-4 gap-2">
                    {PROVIDERS.map((p) => (
                      <button
                        key={p.value}
                        type="button"
                        onClick={() => field.handleChange(p.value)}
                        className={cn(
                          "flex flex-col items-center gap-1.5 rounded-lg border p-3 text-xs font-medium transition-colors",
                          field.state.value === p.value
                            ? "border-primary bg-primary/10 text-foreground"
                            : "text-muted-foreground hover:bg-accent/50",
                        )}
                      >
                        <ProviderIcon provider={p.value} className="size-5" />
                        {p.label}
                      </button>
                    ))}
                  </div>
                </div>
              )}
            />

            {/* Visibility */}
            <form.Field
              name="isPublic"
              children={(field) => (
                <div className="space-y-2">
                  <Label>Visibility</Label>
                  <div className="grid grid-cols-2 gap-2">
                    <button
                      type="button"
                      onClick={() => field.handleChange(false)}
                      className={cn(
                        "flex flex-col gap-1 rounded-lg border p-3 text-left transition-colors",
                        !field.state.value
                          ? "border-primary bg-primary/10"
                          : "hover:bg-accent/50",
                      )}
                    >
                      <span className="flex items-center gap-1.5 text-sm font-medium">
                        <Lock className="h-3.5 w-3.5" /> Private
                      </span>
                      <span className="text-muted-foreground text-xs">
                        Only you (this admin account) can connect via this
                        provider
                      </span>
                    </button>
                    <button
                      type="button"
                      onClick={() => field.handleChange(true)}
                      className={cn(
                        "flex flex-col gap-1 rounded-lg border p-3 text-left transition-colors",
                        field.state.value
                          ? "border-primary bg-primary/10"
                          : "hover:bg-accent/50",
                      )}
                    >
                      <span className="flex items-center gap-1.5 text-sm font-medium">
                        <Users className="h-3.5 w-3.5" /> Public
                      </span>
                      <span className="text-muted-foreground text-xs">
                        Any team member can connect their own account
                      </span>
                    </button>
                  </div>
                </div>
              )}
            />

            {isGitHub ? (
              <>
                <form.Field
                  name="org"
                  children={(field) => (
                    <div className="space-y-2">
                      <Label htmlFor="gh-org">
                        Organization{" "}
                        <span className="text-muted-foreground font-normal">
                          optional
                        </span>
                      </Label>
                      <Input
                        id="gh-org"
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder="acme-corp"
                      />
                      <p className="text-muted-foreground text-xs">
                        Scopes the GitHub App to an org. Leave blank for a
                        personal installation.
                      </p>
                    </div>
                  )}
                />
                {isPublic && (
                  <p className="bg-primary/10 text-foreground rounded-md px-3 py-2 text-xs">
                    Setting visibility to <strong>Public</strong> makes the
                    GitHub App installable by{" "}
                    <strong>any GitHub account</strong>, not just yours.
                  </p>
                )}
                <div className="bg-muted/40 flex items-center justify-between rounded-lg border p-3">
                  <div className="space-y-0.5">
                    <p className="text-sm font-medium">Create GitHub App</p>
                    <p className="text-muted-foreground text-xs">
                      You'll be redirected to GitHub to create and install the
                      App. The provider is configured automatically on return.
                    </p>
                  </div>
                  <Button
                    type="button"
                    onClick={startGitHubApp}
                    className="shrink-0"
                  >
                    <Github className="mr-1 h-4 w-4" /> Create GitHub App
                  </Button>
                </div>
              </>
            ) : (
              <>
                {meta.selfHosted && (
                  <form.Field
                    name="baseUrl"
                    validators={{
                      onChangeListenTo: ["provider"],
                      onChange: ({ value }) =>
                        meta.baseUrlRequired && !value.trim()
                          ? `Base URL is required for ${meta.label}`
                          : undefined,
                    }}
                    children={(field) => {
                      const error = fieldError(field.state.meta.errors);
                      return (
                        <Field data-invalid={!!error}>
                          <FieldLabel htmlFor="base-url">
                            Base URL{" "}
                            <span className="text-muted-foreground font-normal">
                              {meta.baseUrlRequired
                                ? "required"
                                : "self-hosted only"}
                            </span>
                          </FieldLabel>
                          <Input
                            id="base-url"
                            value={field.state.value}
                            onBlur={field.handleBlur}
                            onChange={(e) => field.handleChange(e.target.value)}
                            placeholder={`https://${provider}.yourcompany.com`}
                            aria-invalid={!!error}
                          />
                          {error ? (
                            <FieldError>{error}</FieldError>
                          ) : (
                            !meta.baseUrlRequired && (
                              <p className="text-muted-foreground text-xs">
                                Leave blank to use the cloud-hosted {meta.label}
                                .
                              </p>
                            )
                          )}
                        </Field>
                      );
                    }}
                  />
                )}
                <form.Field
                  name="clientId"
                  validators={{
                    onChange: z.string().min(1, "Client ID is required"),
                  }}
                  children={(field) => {
                    const error = fieldError(field.state.meta.errors);
                    return (
                      <Field data-invalid={!!error}>
                        <FieldLabel htmlFor="client-id">Client ID</FieldLabel>
                        <Input
                          id="client-id"
                          value={field.state.value}
                          onBlur={field.handleBlur}
                          onChange={(e) => field.handleChange(e.target.value)}
                          placeholder="your-client-id"
                          aria-invalid={!!error}
                        />
                        {error && <FieldError>{error}</FieldError>}
                      </Field>
                    );
                  }}
                />
                <form.Field
                  name="clientSecret"
                  validators={{
                    onChange: z.string().min(1, "Client secret is required"),
                  }}
                  children={(field) => {
                    const error = fieldError(field.state.meta.errors);
                    return (
                      <Field data-invalid={!!error}>
                        <FieldLabel htmlFor="client-secret">
                          Client secret
                        </FieldLabel>
                        <div className="relative">
                          <Input
                            id="client-secret"
                            type={showSecret ? "text" : "password"}
                            value={field.state.value}
                            onBlur={field.handleBlur}
                            onChange={(e) => field.handleChange(e.target.value)}
                            placeholder="••••••••••••••••"
                            aria-invalid={!!error}
                          />
                          <button
                            type="button"
                            onClick={() => setShowSecret((s) => !s)}
                            className="text-muted-foreground hover:text-foreground absolute top-1/2 right-2 -translate-y-1/2 text-xs"
                          >
                            {showSecret ? "Hide" : "Show"}
                          </button>
                        </div>
                        {error ? (
                          <FieldError>{error}</FieldError>
                        ) : (
                          <p className="text-muted-foreground text-xs">
                            Stored encrypted — never returned in plaintext after
                            saving.
                          </p>
                        )}
                      </Field>
                    );
                  }}
                />
                <div className="space-y-2">
                  <Label>
                    Callback URL{" "}
                    <span className="text-muted-foreground font-normal">
                      register this on {meta.label}
                    </span>
                  </Label>
                  <div className="flex items-center gap-2">
                    <code className="bg-muted min-w-0 flex-1 truncate rounded px-3 py-2 text-xs">
                      {callbackUrl}
                    </code>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      className="shrink-0"
                      onClick={copyCallback}
                    >
                      {copied ? (
                        <Check className="h-4 w-4" />
                      ) : (
                        <Copy className="h-4 w-4" />
                      )}
                    </Button>
                  </div>
                  <p className="text-muted-foreground text-xs">
                    Add this as the OAuth redirect URI in your {meta.label}
                    application settings.
                  </p>
                </div>
              </>
            )}
          </div>

          <DialogFooter>
            <Button type="button" variant="ghost" onClick={close}>
              Cancel
            </Button>
            {!isGitHub && (
              <Button type="submit" disabled={save.isPending}>
                Add provider
              </Button>
            )}
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
