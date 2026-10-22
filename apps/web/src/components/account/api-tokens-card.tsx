import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { z } from "zod";
import { toast } from "sonner";
import { KeyIcon, Trash2Icon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { fieldError } from "@/lib/utils/field-error";
import { CopyRow } from "@/lib/components/copy-row";
import { ProjectPinSelect } from "@/lib/components/project-pin-select";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "@/components/ui/segmented-control";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Tooltip,
  TooltipContent,
  TooltipPositioner,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  useCreateToken,
  useDeleteToken,
  useTokens,
} from "@/lib/hooks/use-tokens";
import { useProjects } from "@/lib/hooks/use-projects";
import { formatDateTimeShort, formatRelativeTime } from "@/lib/utils/format";
import type { TokenScope, ApiToken } from "@/lib/types";

// Mirrors the backend's validTokenExpiryDays exactly — the API rejects
// anything else, so offering it here would just be a confusing round trip.
const EXPIRY_OPTIONS = [
  { value: "1", label: "1 day" },
  { value: "7", label: "7 days" },
  { value: "14", label: "14 days" },
  { value: "30", label: "30 days" },
  { value: "60", label: "60 days" },
  { value: "90", label: "90 days" },
  { value: "never", label: "No expiry" },
];

// Mirrors middleware.scopeGrants, which is a true total order:
// metrics ⊂ read ⊂ deploy ⊂ write. Every rung includes everything narrower
// than it — there is no combination where e.g. "Read + Deploy" is anything
// other than plain Deploy — so the token only ever needs ONE rung, not a
// set. Order here is narrowest first, matching the picker.
const SCOPE_OPTIONS: {
  value: TokenScope;
  label: string;
  description: string;
}[] = [
  {
    value: "metrics",
    label: "Metrics",
    description:
      "Read metrics only. The narrowest option — for a monitoring scraper.",
  },
  {
    value: "read",
    label: "Read",
    description:
      "View projects, applications, and their data. Includes metrics.",
  },
  {
    value: "deploy",
    label: "Deploy",
    description:
      "Trigger deploys, restarts, and other runtime actions. Includes everything Read can do — but not general write access.",
  },
  {
    value: "write",
    label: "Write",
    description:
      "Create, update, and configure — full access. Includes everything Deploy can do.",
  },
];

// Mirrors middleware.scopeGrants inverted: what a token holding scope X can
// also do, not just what it was literally minted with. Used for the list
// badges so e.g. a Write token doesn't read as narrower than it is — under-
// reporting capability is exactly what made the old checkbox picker
// misleading in the first place.
const SCOPE_GRANTS: Record<TokenScope, TokenScope[]> = {
  write: ["write", "deploy", "read", "metrics"],
  deploy: ["deploy", "read", "metrics"],
  read: ["read", "metrics"],
  metrics: ["metrics"],
};
const SCOPE_DISPLAY_ORDER = SCOPE_OPTIONS.map((o) => o.value);

function effectiveScopes(scopes: TokenScope[]): TokenScope[] {
  const set = new Set<TokenScope>();
  for (const scope of scopes) {
    for (const granted of SCOPE_GRANTS[scope] ?? [scope]) set.add(granted);
  }
  // TokenScope is a compile-time union, not a runtime guarantee — a token
  // carrying a scope this build doesn't know about must still show SOMETHING
  // rather than silently rendering as "no access": append it after the known
  // rungs instead of letting the filter below drop it.
  const known = SCOPE_DISPLAY_ORDER.filter((s) => set.has(s));
  const unknown = [...set].filter((s) => !SCOPE_DISPLAY_ORDER.includes(s));
  return [...known, ...unknown];
}

export function ApiTokensCard() {
  const { data: tokens, isLoading } = useTokens();
  // Held here, not inside CreateTokenDialog: the dialog closes on success, and
  // the plaintext is shown exactly once — unmounting the dialog must not lose it.
  const [issued, setIssued] = useState<string | null>(null);
  // Named for the pin badge below — a token only stores project_ids, not
  // project names, and this list is already fetched for the create dialog's
  // own picker, so resolving it here costs nothing extra (react-query shares
  // the cached result).
  const { data: projects } = useProjects();
  const projectNames = new Map((projects ?? []).map((p) => [p.id, p.name]));

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <KeyIcon aria-hidden="true" className="size-4" />
          Personal Access Tokens
        </CardTitle>
        <CardDescription>
          Tokens authenticate as you, scoped to whatever you choose when
          creating one — for all projects you have access to.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {isLoading ? (
          <div className="space-y-3">
            {[1, 2].map((i) => (
              <div key={i} className="bg-muted h-12 animate-pulse rounded" />
            ))}
          </div>
        ) : tokens && tokens.length > 0 ? (
          <div className="divide-border divide-y">
            {tokens.map((token) => (
              <TokenRow
                key={token.id}
                token={token}
                projectNames={projectNames}
                projectsLoaded={projects !== undefined}
              />
            ))}
          </div>
        ) : (
          <p className="text-muted-foreground text-sm">
            No tokens yet — create one to script against this instance.
          </p>
        )}

        <CreateTokenDialog onIssued={setIssued} />
      </CardContent>

      <Dialog
        open={issued !== null}
        onOpenChange={(next) => !next && setIssued(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Your new token</DialogTitle>
            <DialogDescription>
              This is the only time it is shown. Copy it now — closing this
              dialog does not revoke it, but there is no way to see it again.
            </DialogDescription>
          </DialogHeader>
          {issued && <CopyRow value={issued} />}
          <DialogFooter>
            <Button onClick={() => setIssued(null)}>Done</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}

function TokenRow({
  token,
  projectNames,
  projectsLoaded,
}: {
  token: ApiToken;
  projectNames: Map<string, string>;
  // Distinguishes "the project list hasn't loaded yet" from "it loaded and
  // resolved to zero names" — without this, a pinned token briefly reads as
  // "Pinned: none" (normally reserved for every pinned project having been
  // deleted) on every cold load, before projectNames has anything in it.
  projectsLoaded: boolean;
}) {
  const deleteToken = useDeleteToken();

  const handleDelete = () => {
    toast.promise(deleteToken.mutateAsync(token.id), {
      loading: "Revoking token...",
      success: "Token revoked",
      error: (err) => err.message,
    });
  };

  return (
    <div className="flex items-center justify-between gap-4 py-3 first:pt-0 last:pb-0">
      <div className="min-w-0 space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="truncate text-sm font-medium">{token.name}</span>
          {effectiveScopes(token.scopes).map((scope) => (
            <Badge
              key={scope}
              variant="secondary"
              className="text-xs capitalize"
            >
              {scope}
            </Badge>
          ))}
          {!token.expires_at && (
            <Badge variant="outline" className="text-xs">
              Never expires
            </Badge>
          )}
          {token.pinned && (
            <Badge variant="outline" className="text-xs">
              {/* Zero names resolved (every pinned project deleted, or the
                  owner lost access to all of them) reads as "Pinned: none" —
                  distinct from "Pinned" alone, so a token that reaches
                  nothing doesn't look identical to one with a single,
                  merely-unresolved pin. Gated on projectsLoaded so the SAME
                  "zero names" state during the project list's own loading
                  window doesn't show that same "none" before there was
                  anything to resolve against. */}
              Pinned
              {projectsLoaded &&
                (() => {
                  const names = token.project_ids
                    .map((id) => projectNames.get(id))
                    .filter((name): name is string => !!name);
                  if (names.length === 0) return ": none";
                  return `: ${names.join(", ")}`;
                })()}
            </Badge>
          )}
        </div>
        <p className="text-muted-foreground text-xs">
          {token.expires_at
            ? `Expires ${formatDateTimeShort(token.expires_at)}`
            : "No expiration"}
          {" · "}
          {token.last_used_at
            ? `Last used ${formatRelativeTime(token.last_used_at)}`
            : "Never used"}
        </p>
      </div>

      <AlertDialog>
        <Tooltip>
          <TooltipTrigger
            render={
              <AlertDialogTrigger
                render={
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={`Revoke ${token.name}`}
                    className="text-destructive hover:bg-destructive/10 hover:text-destructive shrink-0"
                  />
                }
              />
            }
          >
            <Trash2Icon aria-hidden="true" className="size-4" />
          </TooltipTrigger>
          <TooltipPositioner>
            <TooltipContent>Revoke</TooltipContent>
          </TooltipPositioner>
        </Tooltip>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Revoke {token.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              Anything using this token stops working immediately. This cannot
              be undone — create a new token first if something still depends on
              this one.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive-solid"
              onClick={handleDelete}
            >
              Revoke
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function CreateTokenDialog({
  onIssued,
}: {
  onIssued: (token: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const createToken = useCreateToken();

  const form = useForm({
    defaultValues: {
      name: "",
      expiry: "30",
      // Write by default — the full-access behavior tokens have always had.
      // Every rung already includes everything narrower than it (see
      // SCOPE_GRANTS), so there is exactly one value here, not a set.
      scope: "write" as TokenScope,
      // Unpinned by default — pinning is opt-in, never the assumed choice.
      // Empty is the unpinned state itself now, not a sentinel translated
      // into one: a multi-select's natural "nothing checked" already means
      // exactly that.
      projectIds: [] as string[],
    },
    onSubmit: async ({ value }) => {
      try {
        const result = await createToken.mutateAsync({
          name: value.name.trim(),
          scopes: [value.scope],
          expiresInDays:
            value.expiry === "never" ? undefined : Number(value.expiry),
          projectIds:
            value.projectIds.length === 0 ? undefined : value.projectIds,
        });
        onIssued(result.token);
        form.reset();
        setOpen(false);
      } catch (err) {
        toast.error(
          err instanceof Error ? err.message : "Could not create token",
        );
      }
    },
  });

  return (
    <>
      <Button onClick={() => setOpen(true)}>Create token</Button>

      <Dialog
        open={open}
        onOpenChange={(next) => {
          if (!next) form.reset();
          setOpen(next);
        }}
      >
        <DialogContent>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              e.stopPropagation();
              form.handleSubmit();
            }}
          >
            <DialogHeader>
              <DialogTitle>Create a personal access token</DialogTitle>
              <DialogDescription>
                Name it after what will use it — you'll want to tell tokens
                apart later, not just when this one is unused.
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4 py-4">
              <form.Field
                name="name"
                validators={{
                  onChange: z.string().min(1, "Name is required"),
                }}
                children={(field) => {
                  const error = fieldError(field.state.meta.errors);
                  return (
                    <Field data-invalid={!!error}>
                      <FieldLabel htmlFor="token-name">Name</FieldLabel>
                      <Input
                        id="token-name"
                        autoFocus
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder="e.g. CI deploy"
                        aria-invalid={!!error}
                      />
                      {error && <FieldError>{error}</FieldError>}
                    </Field>
                  );
                }}
              />
              <form.Field
                name="expiry"
                children={(field) => (
                  <div className="space-y-2">
                    <Label>Expiration</Label>
                    <Select
                      value={field.state.value}
                      onValueChange={(v) => v && field.handleChange(v)}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {EXPIRY_OPTIONS.map((o) => (
                          <SelectItem key={o.value} value={o.value}>
                            {o.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                )}
              />
              <form.Field
                name="scope"
                children={(field) => (
                  <div className="space-y-2">
                    <Label>Scope</Label>
                    <SegmentedControl
                      value={field.state.value}
                      onValueChange={(v) =>
                        v && field.handleChange(v as TokenScope)
                      }
                      fullWidth
                    >
                      {SCOPE_OPTIONS.map((o) => (
                        <SegmentedControlItem key={o.value} value={o.value}>
                          {o.label}
                        </SegmentedControlItem>
                      ))}
                    </SegmentedControl>
                    <p className="text-muted-foreground text-xs">
                      {
                        SCOPE_OPTIONS.find((o) => o.value === field.state.value)
                          ?.description
                      }
                    </p>
                  </div>
                )}
              />
              <form.Field
                name="projectIds"
                children={(field) => (
                  <ProjectPinSelect
                    value={field.state.value}
                    onValueChange={(v) => field.handleChange(v)}
                    helperText='Narrows the token to exactly these projects instead of everything you can reach. Leave empty for every project. If every project a pinned token holds is later deleted, the token reaches nothing — it is never widened back to "every project."'
                  />
                )}
              />
            </div>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => {
                  form.reset();
                  setOpen(false);
                }}
              >
                Cancel
              </Button>
              <form.Subscribe
                selector={(s) => s.canSubmit}
                children={(canSubmit) => (
                  <Button
                    type="submit"
                    disabled={createToken.isPending || !canSubmit}
                  >
                    {createToken.isPending ? "Creating..." : "Create"}
                  </Button>
                )}
              />
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </>
  );
}
