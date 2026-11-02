import { api } from "./client";

export type ChannelType =
  "discord" | "telegram" | "slack" | "webhook" | "ntfy" | "gotify" | "email";

export type EventSeverity = "ok" | "warn" | "error";

/** One subscribable event, served from the Go event registry (no TS drift). */
export interface NotificationEvent {
  type: string;
  label: string;
  description: string;
  severity: EventSeverity;
  group: string;
}

/** A configured channel. The provider config is never returned (secret-free). */
export interface NotificationChannel {
  id: string;
  name: string;
  type: ChannelType;
  events: string[];
  enabled: boolean;
  last_sent_at: string | null;
  last_error: string | null;
  /** Label of the most recently delivered/failed event, or null if none yet. */
  last_event: string | null;
  /** Connection config with secrets stripped, for prefilling the edit form. */
  config?: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

/** Create/update payload. `config` is a per-provider object; omit on update to
 * preserve the stored config. */
export interface SaveNotificationChannel {
  name: string;
  type: ChannelType;
  events: string[];
  enabled: boolean;
  config?: Record<string, unknown>;
}

export interface TestResult {
  ok: boolean;
  error?: string;
}

export function listNotificationEvents() {
  return api.get<NotificationEvent[]>("/notification-events");
}

export function listNotificationChannels() {
  return api.get<NotificationChannel[]>("/notification-channels");
}

export function createNotificationChannel(data: SaveNotificationChannel) {
  return api.post<NotificationChannel>("/notification-channels", data);
}

export function updateNotificationChannel(
  id: string,
  data: SaveNotificationChannel,
) {
  return api.put<NotificationChannel>(`/notification-channels/${id}`, data);
}

export function setNotificationChannelEnabled(id: string, enabled: boolean) {
  return api.patch<NotificationChannel>(`/notification-channels/${id}`, {
    enabled,
  });
}

export function deleteNotificationChannel(id: string) {
  return api.delete<{ status: string }>(`/notification-channels/${id}`);
}

export function testNotificationChannel(id: string) {
  return api.post<TestResult>(`/notification-channels/${id}/test`, {});
}

/** Ad-hoc test from the create/edit form, before the channel is saved. Pass `id`
 * so an edit with blank config falls back to the stored config. */
export function testNotificationChannelParams(data: {
  id?: string;
  type: ChannelType;
  config?: Record<string, unknown>;
}) {
  return api.post<TestResult>("/notification-channels/test", data);
}
