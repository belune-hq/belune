import type { ReactNode } from "react";
import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/utils";

type Tone = "default" | "ready" | "attention" | "error";

const VALUE_TONE: Record<Tone, string> = {
  default: "text-foreground",
  ready: "text-status-ready",
  attention: "text-status-building",
  error: "text-status-error",
};

/** One at-a-glance metric: label, large value, optional hint, footer and tone. */
export function StatCard({
  label,
  value,
  hint,
  tone = "default",
  icon,
  footer,
}: {
  label: string;
  value: ReactNode;
  hint?: ReactNode;
  tone?: Tone;
  icon?: ReactNode;
  footer?: ReactNode;
}) {
  return (
    <Card className="h-full">
      <CardContent className="flex h-full flex-col px-4 py-3">
        <div className="text-text-faint flex items-center gap-1.5">
          {icon}
          <p className="text-xs font-medium tracking-wide uppercase">{label}</p>
        </div>
        <p
          className={cn(
            "mt-2 text-2xl font-semibold tracking-tight",
            VALUE_TONE[tone],
          )}
        >
          {value}
        </p>
        {hint != null && (
          <p className="text-muted-foreground mt-0.5 truncate text-xs first-letter:uppercase">
            {hint}
          </p>
        )}
        {footer != null && (
          <div className="mt-auto flex flex-wrap items-center gap-1.5 pt-3 capitalize">
            {footer}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

/**
 * Graph/meter card: shares the StatCard header chrome (icon + uppercase label,
 * optional right-aligned `aside`) but leaves the body open for a sparkline,
 * bar, meters, or a value — used where a value+description+badge doesn't fit.
 */
export function MetricCard({
  label,
  icon,
  aside,
  children,
  className,
}: {
  label: string;
  icon?: ReactNode;
  /** Right-aligned header node (caller styles it — e.g. a colored status). */
  aside?: ReactNode;
  children?: ReactNode;
  className?: string;
}) {
  return (
    <Card className={cn("h-full", className)}>
      <CardContent className="flex h-full flex-col px-4 py-3">
        <div className="flex items-center justify-between gap-2">
          <div className="text-text-faint flex items-center gap-1.5">
            {icon}
            <p className="text-xs font-medium tracking-wide uppercase">
              {label}
            </p>
          </div>
          {aside}
        </div>
        {children}
      </CardContent>
    </Card>
  );
}

/** A labelled usage meter (used / total) for the host-resources card. */
export function MeterRow({
  label,
  percent,
  detail,
}: {
  label: string;
  percent: number;
  detail?: string;
}) {
  const clamped = Math.max(0, Math.min(100, percent));
  const tone =
    clamped >= 90
      ? "bg-status-error"
      : clamped >= 75
        ? "bg-status-building"
        : "bg-status-ready";
  return (
    <div>
      <div className="mb-1 flex items-baseline justify-between text-xs">
        <span className="text-text-faint">{label}</span>
        <span className="font-mono">
          {detail ? `${detail} · ` : ""}
          {clamped.toFixed(0)}%
        </span>
      </div>
      <div
        className="bg-elev h-1.5 overflow-hidden rounded-full"
        role="progressbar"
        aria-label={label}
        aria-valuenow={Math.round(clamped)}
        aria-valuemin={0}
        aria-valuemax={100}
      >
        <div
          className={cn("h-full rounded-full transition-all", tone)}
          style={{ width: `${clamped}%` }}
        />
      </div>
    </div>
  );
}
