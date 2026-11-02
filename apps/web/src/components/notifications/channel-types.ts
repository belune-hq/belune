import type { ComponentType } from "react";
import { Slack, Bell, Webhook, Mail } from "lucide-react";
import { SiDiscord, SiTelegram, SiNtfy } from "@icons-pack/react-simple-icons";
import type { ChannelType } from "@/lib/api/notification-channels";

// Ordered list backing the type Select in the form dialog.
export const CHANNEL_TYPES: { value: ChannelType; label: string }[] = [
  { value: "discord", label: "Discord" },
  { value: "slack", label: "Slack" },
  { value: "telegram", label: "Telegram" },
  { value: "ntfy", label: "ntfy" },
  { value: "gotify", label: "Gotify" },
  { value: "webhook", label: "Webhook" },
  { value: "email", label: "Email" },
];

export const TYPE_LABEL: Record<ChannelType, string> = Object.fromEntries(
  CHANNEL_TYPES.map((t) => [t.value, t.label]),
) as Record<ChannelType, string>;

// Branded marks (Discord/Telegram/ntfy) come from simple-icons; Slack and Gotify
// aren't in the pack, so they fall back to lucide glyphs, as do the unbranded
// webhook/email types.
export const TYPE_ICON: Record<
  ChannelType,
  ComponentType<{ className?: string }>
> = {
  discord: SiDiscord,
  slack: Slack,
  telegram: SiTelegram,
  webhook: Webhook,
  ntfy: SiNtfy,
  gotify: Bell,
  email: Mail,
};
