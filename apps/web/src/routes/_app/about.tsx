import { createFileRoute } from "@tanstack/react-router";
import { SiGithub } from "@icons-pack/react-simple-icons";
import { BeluneLogo } from "@/lib/components/belune-logo";
import { BRAND } from "@/lib/brand";
import { useVersion } from "@/lib/hooks/use-version";
import { Card, CardContent } from "@/components/ui/card";
import { RouteError } from "@/lib/components/route-error";

export const Route = createFileRoute("/_app/about")({
  component: AboutPage,
  errorComponent: RouteError,
});

const GITHUB_URL = "https://github.com/belune-hq/belune";

function AboutPage() {
  const version = useVersion();

  return (
    <div className="flex min-h-[70vh] items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardContent className="flex flex-col items-center gap-5 py-10 text-center">
          {/* Brand tile, matching the sidebar identity block. */}
          <div
            aria-hidden="true"
            className="grid size-16 place-items-center rounded-2xl text-brand-fg shadow-sm"
            style={{
              background:
                "linear-gradient(140deg, var(--brand), var(--brand-press))",
            }}
          >
            <BeluneLogo className="size-11" />
          </div>

          <p className="text-text-faint text-sm">Designed by <b>Winnie Ha</b></p>

          <div className="space-y-0.5">
            <h1 className="text-xl font-semibold">{BRAND.name}</h1>
            {version && (
              <p className="text-text-faint font-mono text-xs">{version}</p>
            )}
          </div>

          <a
            href={GITHUB_URL}
            target="_blank"
            rel="noreferrer noopener"
            className="text-text-faint hover:text-foreground inline-flex items-center gap-2 text-sm transition-colors"
          >
            <SiGithub className="size-4" aria-hidden="true" />
            github.com/belune-hq/belune
          </a>
        </CardContent>
      </Card>
    </div>
  );
}
