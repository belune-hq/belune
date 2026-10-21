import { create } from "zustand";

interface BreadcrumbState {
  /** Dynamic display names keyed by entity id (projectId, applicationId, …). */
  labels: Record<string, string>;
  setLabel: (id: string, label: string) => void;
}

/**
 * Holds dynamic breadcrumb labels (project / app / database names) keyed by id.
 * The Topbar builds the breadcrumb structure from the current route and looks
 * up display names here, so trails are always correct on navigation (including
 * the browser back button) and stale ids are simply never read.
 */
export const useBreadcrumbStore = create<BreadcrumbState>((set) => ({
  labels: {},
  setLabel: (id, label) =>
    set((s) =>
      s.labels[id] === label
        ? s
        : { labels: { ...s.labels, [id]: label } },
    ),
}));
