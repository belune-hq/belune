import { useEffect, useState } from "react";
import { createFileRoute, Outlet, redirect } from "@tanstack/react-router";
import { Sidebar } from "@/lib/components/layout/sidebar";
import { Topbar } from "@/lib/components/layout/topbar";
import { useAuthStore } from "@/lib/stores/auth";
import { useWebSocketStatus } from "@/lib/hooks/use-websocket";

export const Route = createFileRoute("/_app")({
  beforeLoad: ({ location }) => {
    const { isAuthenticated } = useAuthStore.getState();
    if (!isAuthenticated) {
      throw redirect({ to: "/login", search: { redirect: location.href } });
    }
  },
  component: AppLayout,
});

function AppLayout() {
  const wsStatus = useWebSocketStatus();
  const [mobileNavOpen, setMobileNavOpen] = useState(false);

  // Close the off-canvas drawer on Escape.
  useEffect(() => {
    if (!mobileNavOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMobileNavOpen(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [mobileNavOpen]);

  return (
    <div className="flex h-screen flex-col">
      {wsStatus === "failed" && (
        <div className="border-destructive/20 bg-destructive/10 text-destructive flex items-center justify-center gap-3 border-b px-4 py-2 text-sm">
          <span>Real-time connection lost. Live updates are unavailable.</span>
          <button
            className="underline underline-offset-2"
            onClick={() => window.location.reload()}
          >
            Reload
          </button>
        </div>
      )}
      <div className="flex min-h-0 flex-1">
        {mobileNavOpen && (
          <div
            className="fixed inset-0 z-40 bg-black/50 md:hidden"
            aria-hidden="true"
            onClick={() => setMobileNavOpen(false)}
          />
        )}
        <Sidebar
          mobileOpen={mobileNavOpen}
          onMobileClose={() => setMobileNavOpen(false)}
        />
        <div className="flex min-w-0 flex-1 flex-col">
          <Topbar onMobileMenu={() => setMobileNavOpen(true)} />
          <main className="flex-1 overflow-y-auto p-4 md:p-6">
            <Outlet />
          </main>
        </div>
      </div>
    </div>
  );
}
