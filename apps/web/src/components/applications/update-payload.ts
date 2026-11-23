import type { updateApplication } from "@/lib/api/applications";

/** The source fields the settings form edits. */
export interface SourceFormValues {
  name: string;
  source_repo: string;
  source_image: string;
  dockerfile_path: string;
  root_directory: string;
  build_type_override: string;
  git_token: string;
  registry_username?: string;
  registry_password?: string;
  clear_registry_credentials?: boolean;
  branch: string;
}

/** Which of the two source paths the form is showing, and the connection it
 *  has selected on the git path. */
export interface SourceSelection {
  isGit: boolean;
  gitSource: "connection" | "url";
  gitIntegrationId: string;
}

/** Taken from the API function rather than redeclared, so a field added or
 *  renamed there cannot drift out of sync here. */
type UpdatePayload = Parameters<typeof updateApplication>[2];

/** Builds the PUT body for the application settings form.
 *
 *  ⚠️ The API distinguishes ABSENT (keep the stored value) from EMPTY (clear
 *  it), and this form renders the whole source configuration, so it is the
 *  authority on all of it. Every source field is therefore sent as typed,
 *  blank included: blank is a real choice for most of them — no Dockerfile
 *  path, build from the repository root, let the builder decide — and omitting
 *  a field the user had just cleared would discard the edit while still
 *  reporting "Settings saved". That was a live regression (a72927c): the
 *  cleared field came back on reload.
 *
 *  The opposite error is just as easy, which is why the other type's fields are
 *  sent as "" rather than echoed back. validateSource rejects a non-empty
 *  dockerfile_path or build_type_override on an image application, so echoing a
 *  stale git value would make such an app unsavable from a form that does not
 *  render the offending field — a rejection the user has no way to fix. */
export function buildUpdatePayload(
  value: SourceFormValues,
  { isGit, gitSource, gitIntegrationId }: SourceSelection,
): UpdatePayload {
  return {
    name: value.name || undefined,
    registry_username:
      !isGit && !value.clear_registry_credentials
        ? value.registry_username?.trim() || undefined
        : undefined,
    registry_password:
      !isGit && !value.clear_registry_credentials
        ? value.registry_password || undefined
        : undefined,
    clear_registry_credentials:
      !isGit && value.clear_registry_credentials ? true : undefined,
    source_repo: isGit ? value.source_repo : "",
    source_image: isGit ? "" : value.source_image,
    dockerfile_path: isGit ? value.dockerfile_path : "",
    root_directory: isGit ? value.root_directory : "",
    branch: value.branch,
    build_type_override: isGit ? value.build_type_override : "",
    // A token only applies to the public-URL path; a connected account carries
    // its own credentials.
    git_token:
      isGit && gitSource === "url" ? value.git_token || undefined : undefined,
    // Set the integration on the connection path, and clear it ("" = clear)
    // when the app is edited onto a plain URL. Omitted for image apps so it is
    // preserved.
    git_integration_id: isGit
      ? gitSource === "connection"
        ? gitIntegrationId
        : ""
      : undefined,
  };
}
