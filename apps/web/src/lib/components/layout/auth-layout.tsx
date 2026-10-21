import type { ReactNode } from "react";
import {
  GitBranchIcon,
  ShieldCheckIcon,
  ActivityIcon,
} from "lucide-react";
import { BRAND } from "@/lib/brand";
import { BeluneLogo } from "@/lib/components/belune-logo";
import { useInstanceName } from "@/lib/hooks/use-features";
import { useVersion } from "@/lib/hooks/use-version";

const HIGHLIGHTS = [
  { Icon: GitBranchIcon, text: "Deploy from Git or a Docker image" },
  { Icon: ShieldCheckIcon, text: "Automatic TLS on every domain" },
  { Icon: ActivityIcon, text: "Live logs, metrics, and request tracing" },
];

/**
 * Split-screen branded frame for the pre-auth screens (outside the app shell).
 * Left: brand panel with accent gradient (hidden on mobile).
 * Right: centered form column with a compact brand lockup on top for mobile.
 */
export function AuthLayout({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children: ReactNode;
}) {
  const instanceName = useInstanceName();
  const version = useVersion();
  return (
    <div className="grid min-h-screen lg:grid-cols-2">
      {/* Brand panel */}
      <div
        className="relative hidden flex-col justify-between overflow-hidden p-12 text-brand-fg lg:flex"
        style={{
          background:
            "linear-gradient(150deg, var(--brand-press), var(--brand) 55%, var(--brand-hover))",
        }}
      >
        <div className="flex items-center gap-2.5">
          <div className="grid size-9 place-items-center rounded-lg bg-white/15 backdrop-blur">
            <BeluneLogo className="size-7" />
          </div>
          <div className="flex flex-col leading-tight">
            <span className="text-lg font-semibold">{instanceName}</span>
            <span className="text-xs text-brand-fg/60">Effortless deploys.</span>
          </div>
        </div>

        <div className="max-w-sm">
          <h2 className="text-3xl leading-tight font-semibold tracking-tight">
            Deploy like the cloud.
          </h2>
          <ul className="mt-8 space-y-3">
            {HIGHLIGHTS.map(({ Icon, text }) => (
              <li
                key={text}
                className="flex items-center gap-3 text-sm text-brand-fg/90"
              >
                <Icon aria-hidden="true" className="size-4 shrink-0" />
                {text}
              </li>
            ))}
          </ul>
        </div>

        <p className="font-mono text-xs text-brand-fg/60">
          {BRAND.name}
          {version && ` • ${version}`}
        </p>
      </div>

      {/* Form column */}
      <div className="flex items-center justify-center p-6 sm:p-12">
        <div className="w-full max-w-sm">
          {/* Mobile brand lockup */}
          <div className="mb-8 flex items-center gap-2.5 lg:hidden">
            <div
              className="grid size-9 place-items-center rounded-lg text-brand-fg"
              style={{
                background:
                  "linear-gradient(140deg, var(--brand), var(--brand-press))",
              }}
            >
              <BeluneLogo className="size-7" />
            </div>
            <span className="text-lg font-semibold">{instanceName}</span>
          </div>

          <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
          {description && (
            <p className="text-muted-foreground mt-1.5 text-sm">
              {description}
            </p>
          )}
          <div className="mt-8">{children}</div>
        </div>
      </div>
    </div>
  );
}
