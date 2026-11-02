import {
  useEffect,
  useRef,
  useCallback,
  useLayoutEffect,
  useSyncExternalStore,
} from "react";

interface OutboundMessage {
  channel: string;
  event: string;
  data: unknown;
}

interface InboundMessage {
  action: "subscribe" | "unsubscribe";
  channel: string;
}

type MessageHandler = (event: string, data: unknown) => void;

export type ConnectionState =
  "connected" | "connecting" | "disconnected" | "failed";

// Singleton WebSocket state
let ws: WebSocket | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let retryCount = 0;
const MAX_RETRIES = 10;
const listeners = new Map<string, Set<MessageHandler>>();
let connectPromise: Promise<void> | null = null;

let connectionState: ConnectionState = "disconnected";
const stateListeners = new Set<(state: ConnectionState) => void>();

function setConnectionState(state: ConnectionState) {
  if (connectionState === state) return;
  connectionState = state;
  for (const listener of stateListeners) {
    listener(state);
  }
}

function getWsUrl() {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}/api/ws`;
}

function sendJSON(msg: InboundMessage) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(msg));
  }
}

function handleMessage(event: MessageEvent) {
  try {
    const msg: OutboundMessage = JSON.parse(event.data);
    if (msg.event === "error") {
      // The server answers a subscribe it will not honour on the channel it
      // refused (ws.ChannelAuthorizer). It is not data for that channel's
      // handler, which would parse it as a metric point or a log line.
      console.warn(`WebSocket subscription refused: ${msg.channel}`);
      return;
    }
    const handlers = listeners.get(msg.channel);
    if (handlers) {
      for (const handler of handlers) {
        handler(msg.event, msg.data);
      }
    }
  } catch {
    // ignore parse errors
  }
}

function connect(): Promise<void> {
  if (
    ws &&
    (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)
  ) {
    return connectPromise ?? Promise.resolve();
  }

  setConnectionState("connecting");

  connectPromise = new Promise<void>((resolve) => {
    ws = new WebSocket(getWsUrl());

    ws.onopen = () => {
      retryCount = 0;
      // If all listeners were removed while connecting, close cleanly now.
      if (listeners.size === 0) {
        ws?.close();
        return;
      }
      // Re-subscribe to all active channels after a (re)connect.
      for (const channel of listeners.keys()) {
        sendJSON({ action: "subscribe", channel });
      }
      setConnectionState("connected");
      resolve();
    };

    ws.onmessage = handleMessage;

    ws.onclose = () => {
      ws = null;
      connectPromise = null;
      if (listeners.size > 0 && retryCount < MAX_RETRIES) {
        setConnectionState("connecting");
        const delay = Math.min(1000 * 2 ** retryCount, 30000);
        retryCount++;
        reconnectTimer = setTimeout(() => connect(), delay);
      } else if (retryCount >= MAX_RETRIES) {
        setConnectionState("failed");
      } else {
        setConnectionState("disconnected");
      }
    };

    ws.onerror = () => {
      // onclose fires after onerror — state is updated there.
    };
  });

  return connectPromise;
}

function ensureConnected() {
  if (
    !ws ||
    ws.readyState === WebSocket.CLOSED ||
    ws.readyState === WebSocket.CLOSING
  ) {
    connect();
  }
}

function subscribe(channel: string, handler: MessageHandler) {
  let handlers = listeners.get(channel);
  const isNew = !handlers || handlers.size === 0;
  if (!handlers) {
    handlers = new Set();
    listeners.set(channel, handlers);
  }
  handlers.add(handler);

  if (isNew) {
    if (ws?.readyState === WebSocket.OPEN) {
      // Already connected — send the subscribe message immediately.
      sendJSON({ action: "subscribe", channel });
    } else {
      // Not yet connected — ensureConnected starts the connection.
      // onopen will re-subscribe all active channels once connected,
      // so we must NOT call sendJSON here (WS is not OPEN yet).
      ensureConnected();
    }
  }
}

function unsubscribe(channel: string, handler: MessageHandler) {
  const handlers = listeners.get(channel);
  if (!handlers) return;
  handlers.delete(handler);
  if (handlers.size === 0) {
    listeners.delete(channel);
    sendJSON({ action: "unsubscribe", channel });

    // Close WS when no more listeners remain.
    if (listeners.size === 0) {
      if (reconnectTimer) {
        clearTimeout(reconnectTimer);
        reconnectTimer = null;
      }
      retryCount = 0;
      // Only close if OPEN — if CONNECTING, onopen checks listeners.size === 0
      // and closes cleanly to avoid "WebSocket closed before connection established".
      if (ws?.readyState === WebSocket.OPEN) {
        ws.close();
      }
      ws = null;
      connectPromise = null;
      setConnectionState("disconnected");
    }
  }
}

/**
 * Subscribe to a WebSocket channel. The callback receives (event, data) for
 * every message on that channel. Automatically subscribes on mount and
 * unsubscribes on unmount.
 */
export function useChannel<T = unknown>(
  channel: string | null,
  onMessage: (event: string, data: T) => void,
) {
  const onMessageRef = useRef(onMessage);
  // Layout effect, not a plain assignment: mutating a ref during render is
  // disallowed (can desync from a discarded render). This still updates
  // before any WS message could fire — synchronously after commit, before
  // paint — so onMessageRef is never stale when handler() runs.
  useLayoutEffect(() => {
    onMessageRef.current = onMessage;
  });

  const handler = useCallback((event: string, data: unknown) => {
    onMessageRef.current(event, data as T);
  }, []);

  useEffect(() => {
    if (!channel) return;
    subscribe(channel, handler);
    return () => unsubscribe(channel, handler);
  }, [channel, handler]);

  // Derived straight from the same external store as useWebSocketStatus,
  // folding in the local `channel` prop — no local setConnected juggling in
  // the effect above, which used to duplicate this via a stateListener.
  const connected = useSyncExternalStore(
    subscribeToConnectionState,
    () => !!channel && connectionState === "connected",
  );

  return { connected };
}

function subscribeToConnectionState(onStoreChange: () => void) {
  // stateListeners' callbacks take the new state; useSyncExternalStore's
  // subscribe callback takes none and re-reads getSnapshot itself instead,
  // which is what makes this immune to the render/commit race a manual
  // useEffect subscription has to work around by re-syncing on mount.
  const listener = () => onStoreChange();
  stateListeners.add(listener);
  return () => stateListeners.delete(listener);
}

function getConnectionStateSnapshot() {
  return connectionState;
}

/**
 * Returns the current WebSocket connection state. Useful for showing a
 * global "connection lost" banner when the state is "failed".
 */
export function useWebSocketStatus(): ConnectionState {
  return useSyncExternalStore(
    subscribeToConnectionState,
    getConnectionStateSnapshot,
  );
}
