import { createFileRoute } from "@tanstack/react-router";
import { useEffect, useRef, useState, useCallback } from "react";
import { Terminal } from "xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import "xterm/css/xterm.css";
import { createTerminalSession } from "@/lib/api/terminal";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { RefreshCw, TerminalSquare } from "lucide-react";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId/terminal",
)({
  component: TerminalPage,
});

type ConnState = "idle" | "connecting" | "connected" | "disconnected" | "error";

function getWsUrl(sessionId: string) {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}/api/ws/terminal/${sessionId}`;
}

function TerminalPage() {
  const { projectId, applicationId } = Route.useParams();
  const [shell, setShell] = useState("sh");
  const [connState, setConnState] = useState<ConnState>("idle");
  const [errorMsg, setErrorMsg] = useState<string | null>(null);

  const containerRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const sessionIdRef = useRef<string | null>(null);

  const cleanup = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close();
      wsRef.current = null;
    }
    if (termRef.current) {
      termRef.current.dispose();
      termRef.current = null;
    }
    fitRef.current = null;
    sessionIdRef.current = null;
  }, []);

  const connect = useCallback(async () => {
    cleanup();
    setConnState("connecting");
    setErrorMsg(null);

    let sessionId: string;
    try {
      const res = await createTerminalSession(projectId, applicationId, shell);
      sessionId = res.session_id;
    } catch (err: unknown) {
      setConnState("error");
      setErrorMsg(
        err instanceof Error ? err.message : "Failed to create session",
      );
      return;
    }
    sessionIdRef.current = sessionId;

    // Mount xterm into the container div
    if (!containerRef.current) return;
    const term = new Terminal({
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
    const fit = new FitAddon();
    const links = new WebLinksAddon();
    term.loadAddon(fit);
    term.loadAddon(links);
    term.open(containerRef.current);
    fit.fit();
    termRef.current = term;
    fitRef.current = fit;

    // Connect WebSocket
    const ws = new WebSocket(getWsUrl(sessionId));
    wsRef.current = ws;

    ws.onopen = () => {
      setConnState("connected");
      // Send initial resize
      const dims = fit.proposeDimensions();
      if (dims) {
        ws.send(
          JSON.stringify({ type: "resize", cols: dims.cols, rows: dims.rows }),
        );
      }
    };

    ws.onmessage = (event) => {
      try {
        const msg = JSON.parse(event.data as string) as {
          type: string;
          data?: string;
          message?: string;
        };
        if (msg.type === "output" && msg.data) {
          const bytes = atob(msg.data);
          term.write(bytes);
        } else if (msg.type === "closed") {
          setConnState("disconnected");
          term.writeln("\r\n\x1b[33m[Session ended]\x1b[0m");
        } else if (msg.type === "error") {
          setConnState("error");
          setErrorMsg(msg.message ?? "Unknown error");
        }
      } catch {
        // ignore parse errors
      }
    };

    ws.onerror = () => {
      setConnState("error");
      setErrorMsg("WebSocket connection failed");
    };

    ws.onclose = (ev) => {
      if (connState !== "disconnected") {
        setConnState("disconnected");
      }
      if (ev.code !== 1000) {
        term.writeln("\r\n\x1b[31m[Connection closed]\x1b[0m");
      }
    };

    // Terminal input → WebSocket
    term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "input", data: btoa(data) }));
      }
    });

    // Resize observer → WebSocket resize messages
    const resizeObserver = new ResizeObserver(() => {
      if (fitRef.current && termRef.current) {
        fitRef.current.fit();
        const dims = fitRef.current.proposeDimensions();
        if (dims && wsRef.current?.readyState === WebSocket.OPEN) {
          wsRef.current.send(
            JSON.stringify({
              type: "resize",
              cols: dims.cols,
              rows: dims.rows,
            }),
          );
        }
      }
    });
    if (containerRef.current) {
      resizeObserver.observe(containerRef.current);
    }

    // Cleanup on unmount
    return () => {
      resizeObserver.disconnect();
    };
  }, [projectId, applicationId, shell, cleanup, connState]);

  // Cleanup on unmount
  useEffect(() => {
    return () => cleanup();
  }, [cleanup]);

  const isConnected = connState === "connected";
  const isConnecting = connState === "connecting";

  return (
    <div
      className="flex flex-col gap-4"
      style={{ height: "calc(100vh - 280px)" }}
    >
      {/* Toolbar */}
      <div className="flex items-center gap-3">
        <TerminalSquare className="text-muted-foreground h-4 w-4" />
        <Select
          value={shell}
          onValueChange={(v) => v && setShell(v)}
          disabled={isConnected || isConnecting}
        >
          <SelectTrigger className="h-8 w-28 text-sm" aria-label="Shell">
            <SelectValue placeholder="Shell" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="sh" icon={<TerminalSquare />}>
              sh
            </SelectItem>
            <SelectItem value="bash" icon={<TerminalSquare />}>
              bash
            </SelectItem>
            <SelectItem value="ash" icon={<TerminalSquare />}>
              ash
            </SelectItem>
            <SelectItem value="zsh" icon={<TerminalSquare />}>
              zsh
            </SelectItem>
            <SelectItem value="fish" icon={<TerminalSquare />}>
              fish
            </SelectItem>
          </SelectContent>
        </Select>

        {!isConnected && !isConnecting && (
          <Button size="sm" onClick={connect} disabled={isConnecting}>
            {connState === "idle" ? "Connect" : "Reconnect"}
          </Button>
        )}

        {isConnected && (
          <Button
            size="sm"
            variant="outline"
            onClick={cleanup}
            className="gap-1"
          >
            Disconnect
          </Button>
        )}

        {(connState === "disconnected" || connState === "error") && (
          <Button
            size="sm"
            variant="outline"
            onClick={connect}
            className="gap-1"
          >
            <RefreshCw className="h-3.5 w-3.5" />
            Reconnect
          </Button>
        )}

        <span
          role="status"
          aria-live="polite"
          className={`text-xs ${
            isConnected
              ? "text-green-500"
              : isConnecting
                ? "text-yellow-500"
                : connState === "error"
                  ? "text-destructive"
                  : "text-muted-foreground"
          }`}
        >
          {connState === "idle"
            ? "Not connected"
            : connState === "connecting"
              ? "Connecting…"
              : connState === "connected"
                ? "Connected"
                : connState === "disconnected"
                  ? "Disconnected"
                  : `Error${errorMsg ? `: ${errorMsg}` : ""}`}
        </span>
      </div>

      {/* Terminal area */}
      <div
        ref={containerRef}
        role="application"
        aria-label="Application shell terminal"
        className="flex-1 overflow-hidden rounded border border-zinc-800 bg-zinc-950"
        style={{ minHeight: 0 }}
      />

      {connState === "idle" && (
        <div className="pointer-events-none absolute inset-0 flex items-center justify-center">
          <p className="text-muted-foreground text-sm">
            Select a shell and click Connect to open a terminal session.
          </p>
        </div>
      )}
    </div>
  );
}
