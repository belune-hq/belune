import { createFileRoute, redirect } from "@tanstack/react-router";

// Preserve callback outcomes and bookmarks while the canonical page moves.
export const Route = createFileRoute("/_app/git")({
  validateSearch: (search: Record<string, unknown>) => ({
    tab:
      search.tab === "providers" || search.tab === "registries"
        ? search.tab
        : undefined,
    connected:
      typeof search.connected === "string" ? search.connected : undefined,
    error: typeof search.error === "string" ? search.error : undefined,
  }),
  beforeLoad: ({ search }) => {
    throw redirect({ to: "/sources", search, replace: true });
  },
});
