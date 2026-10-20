/**
 * Server branding shown in the sidebar identity block.
 *
 * Single-tenant: one install = one server, so this is static branding, not an
 * org switcher.
 *
 * The version is deliberately NOT here. It used to be a hand-edited literal,
 * which meant a release that forgot to bump it shipped a UI confidently
 * reporting the previous version. It now comes from the running binary via
 * `useVersion()` (GET /api/version), which cannot go stale.
 */
export const BRAND = {
  name: "Belune",
  /** Canonical source repo. Linked from the sidebar's wordmark. */
  repoUrl: "https://github.com/belune-hq/belune",
} as const;
