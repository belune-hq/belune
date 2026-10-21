import type { ReactNode } from "react";
import { Link, useRouter } from "@tanstack/react-router";
import { Button, buttonVariants } from "@/components/ui/button";
import { BeluneLogo } from "@/lib/components/belune-logo";
import { useInstanceName } from "@/lib/hooks/use-features";
import { useAuthStore } from "@/lib/stores/auth";
import { cn } from "@/lib/utils";

/** Centered standalone frame (outside the app shell) with a small brand lockup. */
function StatusPageFrame({
  code,
  title,
  description,
  children,
}: {
  code: string;
  title: string;
  description: string;
  children?: ReactNode;
}) {
  const instanceName = useInstanceName();
  return (
    <div className="flex min-h-screen flex-col items-center justify-center px-6 py-12 text-center">
      <div className="flex items-center gap-2.5">
        <div
          aria-hidden="true"
          className="grid size-8 place-items-center rounded-lg text-brand-fg"
          style={{
            background:
              "linear-gradient(140deg, var(--brand), var(--brand-press))",
          }}
        >
          <BeluneLogo className="size-6" />
        </div>
        <span className="text-sm font-semibold">{instanceName}</span>
      </div>

      <p className="from-text-muted to-text-faint mt-10 bg-gradient-to-b bg-clip-text text-7xl font-bold tracking-tight text-transparent select-none">
        {code}
      </p>
      <h1 className="mt-2 text-2xl font-semibold tracking-tight">{title}</h1>
      <p className="text-muted-foreground mt-2 max-w-md text-sm">
        {description}
      </p>

      <div className="mt-8 w-full max-w-md">{children}</div>
    </div>
  );
}

export function NotFoundPage() {
  const isAuthenticated = useAuthStore((s) => s.isAuthenticated);
  return (
    <StatusPageFrame
      code="404"
      title="Page not found"
      description="The page you're looking for doesn't exist or may have been moved."
    >
      <div className="flex flex-col items-center justify-center gap-2 sm:flex-row">
        <Link
          to={isAuthenticated ? "/projects" : "/login"}
          className={cn(buttonVariants(), "w-full sm:w-auto")}
        >
          {isAuthenticated ? "Back to projects" : "Back to login"}
        </Link>
        {isAuthenticated && (
          <Link
            to="/login"
            className={cn(
              buttonVariants({ variant: "outline" }),
              "w-full sm:w-auto",
            )}
          >
            Back to login
          </Link>
        )}
      </div>
    </StatusPageFrame>
  );
}

export function RootErrorBoundary({ error }: { error: Error }) {
  const router = useRouter();
  const isAuthenticated = useAuthStore((s) => s.isAuthenticated);
  return (
    <StatusPageFrame
      code="Error"
      title="Something went wrong"
      description="An unexpected error occurred. You can retry, or head back and try again."
    >
      <div className="flex flex-col items-center justify-center gap-2 sm:flex-row">
        <Button
          variant="outline"
          className="w-full sm:w-auto"
          onClick={() => router.invalidate()}
        >
          Try again
        </Button>
        <Link
          to={isAuthenticated ? "/projects" : "/login"}
          className={cn(buttonVariants(), "w-full sm:w-auto")}
        >
          {isAuthenticated ? "Back to projects" : "Back to login"}
        </Link>
      </div>

      {error.message && (
        <details className="mt-6 text-left">
          <summary className="text-text-faint hover:text-foreground cursor-pointer text-xs">
            Technical details
          </summary>
          <pre className="bg-elev text-text-muted mt-2 max-h-48 overflow-auto rounded-lg p-3 text-left font-mono text-xs">
            {error.message}
          </pre>
        </details>
      )}
    </StatusPageFrame>
  );
}
