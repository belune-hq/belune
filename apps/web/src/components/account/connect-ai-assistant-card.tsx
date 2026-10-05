import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { z } from "zod";
import { toast } from "sonner";
import { BotIcon, ShieldAlertIcon } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
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
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { fieldError } from "@/lib/utils/field-error";
import { Input } from "@/components/ui/input";
import { CopyRow } from "@/lib/components/copy-row";
import { ProjectPinSelect } from "@/lib/components/project-pin-select";
import { useCreateToken } from "@/lib/hooks/use-tokens";
import type { TokenScope } from "@/lib/types";

// Read is the floor the /mcp route requires (see routes.go) and is enough for
// every tool that only looks. The tools that change things each need a wider
// scope, checked per tool in mcpserver/scope.go; this dialog deliberately never
// mints one — a picker would undercut the point, which is the obvious,
// narrowest-useful token. Someone who wants an assistant to act mints a wider
// token from the general Create Token dialog, knowingly.
const MCP_SCOPE: TokenScope = "read";

function mcpAddCommand(token: string) {
  return `claude mcp add --transport http belune ${window.location.origin}/mcp --header "Authorization: Bearer ${token}"`;
}

/**
 * Mints a Read-scoped personal access token and hands back the ready-to-paste
 * `claude mcp add` command in one step.
 *
 * Consent for what the MCP tools can see (container logs, verbatim, with
 * whatever secrets they happen to contain) lives HERE — at the moment
 * someone actually connects a client — rather than as a warning on every
 * token, most of which are unrelated CI credentials that would only turn the
 * warning into noise.
 */
export function ConnectAIAssistantCard() {
  const [open, setOpen] = useState(false);
  const [command, setCommand] = useState<string | null>(null);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <BotIcon aria-hidden="true" className="size-4" />
          Connect an AI Assistant
        </CardTitle>
        <CardDescription>
          Give an MCP-compatible client read-only access to your projects,
          deployments, and logs through a personal access token scoped to Read.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Button onClick={() => setOpen(true)}>Connect</Button>
      </CardContent>

      <ConnectDialog
        open={open}
        onOpenChange={setOpen}
        onConnected={(token) => setCommand(mcpAddCommand(token))}
      />

      <Dialog
        open={command !== null}
        onOpenChange={(next) => !next && setCommand(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Ready to connect</DialogTitle>
            <DialogDescription>
              Paste this into your terminal. The token is embedded in it and
              shown only this once — copy the whole command now.
            </DialogDescription>
          </DialogHeader>
          {command && <CopyRow value={command} multiline />}
          <DialogFooter>
            <Button onClick={() => setCommand(null)}>Done</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}

function ConnectDialog({
  open,
  onOpenChange,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onConnected: (token: string) => void;
}) {
  const createToken = useCreateToken();

  const form = useForm({
    defaultValues: {
      name: "AI Assistant",
      // Unpinned by default — every project the owner can reach, matching
      // the general Create Token dialog's default. This flow used to mint
      // unconditionally unpinned tokens with no way to narrow them; the
      // picker below is the fix, not a new capability the backend lacked.
      projectIds: [] as string[],
    },
    onSubmit: async ({ value }) => {
      try {
        const result = await createToken.mutateAsync({
          name: value.name.trim(),
          scopes: [MCP_SCOPE],
          // No expiry: this is meant to be a standing integration (a
          // client's saved MCP config), not a short-lived CI credential —
          // the general dialog's 30-day default would silently break it
          // until someone noticed the client stopped working.
          expiresInDays: undefined,
          projectIds:
            value.projectIds.length === 0 ? undefined : value.projectIds,
        });
        form.reset();
        onOpenChange(false);
        onConnected(result.token);
      } catch (err) {
        toast.error(
          err instanceof Error ? err.message : "Could not create token",
        );
      }
    },
  });

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) form.reset();
        onOpenChange(next);
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
            <DialogTitle>Connect an AI Assistant</DialogTitle>
            <DialogDescription>
              Mints a personal access token scoped to Read and gives you the
              command to add this install as an MCP server.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-4">
            <Alert variant="warning">
              <ShieldAlertIcon aria-hidden="true" />
              <AlertTitle>Logs are sent to your client, verbatim</AlertTitle>
              <AlertDescription>
                Tools that read container logs send them to whichever AI client
                you connect. Logs often contain connection strings and API keys
                — only connect a client you trust with that.
              </AlertDescription>
            </Alert>
            <form.Field
              name="name"
              validators={{
                onChange: z.string().min(1, "Name is required"),
              }}
              children={(field) => {
                const error = fieldError(field.state.meta.errors);
                return (
                  <Field data-invalid={!!error}>
                    <FieldLabel htmlFor="mcp-token-name">Token name</FieldLabel>
                    <Input
                      id="mcp-token-name"
                      autoFocus
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                      aria-invalid={!!error}
                    />
                    {error ? (
                      <FieldError>{error}</FieldError>
                    ) : (
                      <p className="text-muted-foreground text-xs">
                        Scoped to Read, no expiry. Revoke it like any other
                        token from Personal Access Tokens above.
                      </p>
                    )}
                  </Field>
                );
              }}
            />
            <form.Field
              name="projectIds"
              children={(field) => (
                <ProjectPinSelect
                  value={field.state.value}
                  onValueChange={(v) => field.handleChange(v)}
                  helperText="Narrows what this client can see to exactly these projects instead of everything you can reach. Leave empty for every project."
                />
              )}
            />
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
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
                  {createToken.isPending ? "Connecting..." : "Connect"}
                </Button>
              )}
            />
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
