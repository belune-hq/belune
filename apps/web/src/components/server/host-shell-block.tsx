import { useEffect, useRef, useState } from "react";
import { useForm } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { TerminalIcon } from "lucide-react";
import { Terminal } from "xterm";
import { FitAddon } from "@xterm/addon-fit";
import "xterm/css/xterm.css";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
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
import { useSettings, useUpdateSettings } from "@/lib/hooks/use-settings";
import { useTotpStatus } from "@/lib/hooks/use-totp";
import { createHostShellSession } from "@/lib/api/maintenance";
import { Switch } from "@/components/ui/switch";

function wsUrl(sessionId: string) {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}/api/ws/terminal/${sessionId}`;
}

export function HostShellBlock() {
  const { data: settings } = useSettings();
  const { data: totpStatus } = useTotpStatus();
  const totpEnabled = totpStatus?.enabled ?? false;
  const updateSettings = useUpdateSettings();
  const enabled =
    settings?.find((s) => s.key === "host_shell_enabled")?.value === "true";

  const [confirmEnable, setConfirmEnable] = useState(false);
  const [passwordOpen, setPasswordOpen] = useState(false);
  const [sessionId, setSessionId] = useState<string | null>(null);

  const setEnabled = (next: boolean) => {
    toast.promise(
      updateSettings.mutateAsync([
        { key: "host_shell_enabled", value: next ? "true" : "false" },
      ]),
      {
        loading: "Saving…",
        success: `Host shell ${next ? "enabled" : "disabled"}`,
        error: (err) => err.message,
      },
    );
  };

  const handleToggle = () => {
    if (enabled) setEnabled(false);
    else setConfirmEnable(true);
  };

  const closePasswordDialog = () => {
    setPasswordOpen(false);
    form.reset();
  };

  const form = useForm({
    defaultValues: { password: "", code: "" },
    onSubmit: async ({ value }) => {
      try {
        const res = await createHostShellSession(value.password, value.code);
        setSessionId(res.session_id);
        closePasswordDialog();
      } catch (err) {
        toast.error(
          err instanceof Error ? err.message : "Failed to open host shell",
        );
      }
    },
  });

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <TerminalIcon className="text-text-muted size-4" />
            <p className="text-sm font-medium">Host Shell</p>
            {enabled && (
              <span className="text-status-error text-xs">root access</span>
            )}
          </div>
          <p className="text-muted-foreground mt-1 text-xs">
            Opens a root shell on the host machine in the browser. Off by
            default; each session re-asks for your password and is written to
            the audit log.
          </p>
        </div>
        <div className="flex items-center gap-2">
          {enabled && (
            <Button
              size="sm"
              variant="outline"
              onClick={() => setPasswordOpen(true)}
            >
              Open Host Shell
            </Button>
          )}
          <Switch
            aria-label="Enable host shell"
            checked={enabled}
            disabled={updateSettings.isPending}
            onCheckedChange={handleToggle}
          />
        </div>
      </div>

      {/* Enable confirmation — this is a serious capability. */}
      <AlertDialog open={confirmEnable} onOpenChange={setConfirmEnable}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Enable host shell?</AlertDialogTitle>
            <AlertDialogDescription>
              This lets any admin open a full{" "}
              <strong>root shell on the host</strong> through the dashboard.
              Opening a session still requires re-entering your password, and
              every session is audited — but the capability is a large increase
              in blast radius. Only enable it if you understand that.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => setEnabled(true)}>
              Enable
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Step-up password prompt. */}
      <Dialog
        open={passwordOpen}
        onOpenChange={(open) => !open && closePasswordDialog()}
      >
        <DialogContent className="sm:max-w-md">
          <form
            onSubmit={(e) => {
              e.preventDefault();
              e.stopPropagation();
              form.handleSubmit();
            }}
            className="space-y-4"
          >
            <DialogHeader>
              <DialogTitle>Confirm it's you</DialogTitle>
              <DialogDescription>
                Re-enter your Belune password to open a root shell on the host.
                {totpEnabled &&
                  " Your authenticator code is required too — this is the most privileged action here."}
              </DialogDescription>
            </DialogHeader>
            <form.Field
              name="password"
              validators={{
                onChange: z.string().min(1, "Password is required"),
              }}
              children={(field) => (
                <Input
                  type="password"
                  autoFocus
                  value={field.state.value}
                  placeholder="Password"
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                />
              )}
            />
            {totpEnabled && (
              <form.Field
                name="code"
                validators={{
                  onChange: ({ value }) =>
                    value ? undefined : "Verification code is required",
                }}
                children={(field) => (
                  <Input
                    inputMode="numeric"
                    autoComplete="one-time-code"
                    value={field.state.value}
                    placeholder="Verification code"
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                  />
                )}
              />
            )}
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={closePasswordDialog}
              >
                Cancel
              </Button>
              <form.Subscribe
                selector={(s) =>
                  [s.values.password, s.values.code, s.isSubmitting] as const
                }
                children={([password, code, isSubmitting]) => (
                  <Button
                    type="submit"
                    disabled={
                      isSubmitting || !password || (totpEnabled && !code)
                    }
                  >
                    {isSubmitting ? "Opening…" : "Open shell"}
                  </Button>
                )}
              />
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* The live terminal. */}
      <Dialog
        open={sessionId !== null}
        onOpenChange={(open) => !open && setSessionId(null)}
      >
        <DialogContent className="sm:max-w-5xl">
          <DialogHeader>
            <DialogTitle>Host shell — root@host</DialogTitle>
          </DialogHeader>
          {sessionId && <HostShellTerminal sessionId={sessionId} />}
        </DialogContent>
      </Dialog>
    </>
  );
}

function HostShellTerminal({ sessionId }: { sessionId: string }) {
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    // The session is single-use: the server tears it down (and removes the host
    // helper) the moment its WebSocket closes. React StrictMode double-invokes
    // effects in dev, so connecting synchronously would open a session, have the
    // throwaway cleanup close it, then leave the real pass reconnecting to a dead
    // session. Defer the connect and cancel it if we unmount first, so exactly one
    // connection is ever made.
    let cancelled = false;
    let ws: WebSocket | null = null;
    let term: Terminal | null = null;
    let ro: ResizeObserver | null = null;

    const timer = setTimeout(() => {
      if (cancelled) return;

      const t = new Terminal({
        cursorBlink: true,
        fontFamily: "monospace",
        fontSize: 13,
        theme: {
          background: "#09090b",
          foreground: "#e4e4e7",
          cursor: "#a1a1aa",
          selectionBackground: "#3f3f46",
        },
      });
      term = t;
      const fit = new FitAddon();
      t.loadAddon(fit);
      t.open(el);
      fit.fit();

      const socket = new WebSocket(wsUrl(sessionId));
      ws = socket;

      const sendResize = () => {
        const dims = fit.proposeDimensions();
        if (dims && socket.readyState === WebSocket.OPEN) {
          socket.send(
            JSON.stringify({
              type: "resize",
              cols: dims.cols,
              rows: dims.rows,
            }),
          );
        }
      };

      socket.onopen = () => {
        t.focus();
        sendResize();
      };
      socket.onmessage = (event) => {
        try {
          const msg = JSON.parse(event.data as string) as {
            type: string;
            data?: string;
            message?: string;
          };
          if (msg.type === "output" && msg.data) {
            t.write(atob(msg.data));
          } else if (msg.type === "closed") {
            t.writeln("\r\n\x1b[33m[Session ended]\x1b[0m");
          } else if (msg.type === "error") {
            t.writeln(`\r\n\x1b[31m[${msg.message ?? "Error"}]\x1b[0m`);
          }
        } catch {
          // ignore parse errors
        }
      };
      socket.onclose = (ev) => {
        if (ev.code !== 1000) {
          t.writeln("\r\n\x1b[31m[Connection closed]\x1b[0m");
        }
      };

      t.onData((data) => {
        if (socket.readyState === WebSocket.OPEN) {
          socket.send(JSON.stringify({ type: "input", data: btoa(data) }));
        }
      });

      const observer = new ResizeObserver(() => {
        fit.fit();
        sendResize();
      });
      ro = observer;
      observer.observe(el);
    }, 0);

    return () => {
      cancelled = true;
      clearTimeout(timer);
      ro?.disconnect();
      ws?.close();
      term?.dispose();
    };
  }, [sessionId]);

  return (
    <div
      ref={containerRef}
      role="application"
      aria-label="Host shell terminal"
      className="h-[60vh] overflow-hidden rounded border border-zinc-800 bg-zinc-950"
    />
  );
}
