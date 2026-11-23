import { describe, expect, it } from "vitest";
import {
  buildUpdatePayload,
  type SourceFormValues,
  type SourceSelection,
} from "./update-payload";

const filled: SourceFormValues = {
  name: "api",
  source_repo: "https://github.com/acme/api.git",
  source_image: "ghcr.io/acme/api:1.2.3",
  dockerfile_path: "docker/Dockerfile",
  root_directory: "services/api",
  build_type_override: "dockerfile",
  git_token: "ghp_token",
  branch: "main",
};

const gitUrl: SourceSelection = {
  isGit: true,
  gitSource: "url",
  gitIntegrationId: "",
};
const gitConnection: SourceSelection = {
  isGit: true,
  gitSource: "connection",
  gitIntegrationId: "int-1",
};
const image: SourceSelection = {
  isGit: false,
  gitSource: "url",
  gitIntegrationId: "",
};

describe("buildUpdatePayload — clearing a field", () => {
  it("sends a cleared optional field as empty, not absent", () => {
    // The regression this covers (a72927c): these were sent as `|| undefined`,
    // which the API reads as "keep the stored value", so clearing a Dockerfile
    // path saved nothing and the old value came back on reload — under a
    // "Settings saved" toast.
    const cleared: SourceFormValues = {
      ...filled,
      dockerfile_path: "",
      root_directory: "",
      build_type_override: "",
      branch: "",
    };
    const payload = buildUpdatePayload(cleared, gitUrl);

    expect(payload.dockerfile_path).toBe("");
    expect(payload.root_directory).toBe("");
    expect(payload.build_type_override).toBe("");
    expect(payload.branch).toBe("");
  });

  it("keeps every cleared source field present as a key", () => {
    // Belt and braces: a value of undefined would serialise away entirely and
    // read as absent even though the assertion above looks satisfied.
    const cleared: SourceFormValues = {
      ...filled,
      dockerfile_path: "",
      root_directory: "",
    };
    const sent = JSON.parse(
      JSON.stringify(buildUpdatePayload(cleared, gitUrl)),
    ) as Record<string, unknown>;

    expect(Object.keys(sent)).toContain("dockerfile_path");
    expect(Object.keys(sent)).toContain("root_directory");
  });

  it("sends a blank name as absent, since an application must keep one", () => {
    // name is the one field where blank is not a meaningful choice.
    const payload = buildUpdatePayload({ ...filled, name: "" }, gitUrl);
    expect(payload.name).toBeUndefined();
  });
});

describe("buildUpdatePayload — the other type's fields", () => {
  it("clears the git fields for an image application", () => {
    // Not echoed back: validateSource rejects a non-empty dockerfile_path or
    // build_type_override on an image app, which would make it unsavable from a
    // form that does not render those fields.
    const payload = buildUpdatePayload(filled, image);

    expect(payload.source_repo).toBe("");
    expect(payload.dockerfile_path).toBe("");
    expect(payload.root_directory).toBe("");
    expect(payload.build_type_override).toBe("");
    expect(payload.source_image).toBe("ghcr.io/acme/api:1.2.3");
  });

  it("clears the image field for a git application", () => {
    const payload = buildUpdatePayload(filled, gitUrl);

    expect(payload.source_image).toBe("");
    expect(payload.source_repo).toBe("https://github.com/acme/api.git");
  });

  it("sends branch as typed for both types", () => {
    // branch was the only field that already got this right, and it applies to
    // an image app too (it is stored, not validated against the type).
    expect(buildUpdatePayload(filled, gitUrl).branch).toBe("main");
    expect(buildUpdatePayload(filled, image).branch).toBe("main");
  });
});

describe("buildUpdatePayload — credentials", () => {
  it("sends a token only on the public-URL git path", () => {
    expect(buildUpdatePayload(filled, gitUrl).git_token).toBe("ghp_token");
    // A connected account carries its own credentials.
    expect(buildUpdatePayload(filled, gitConnection).git_token).toBeUndefined();
    expect(buildUpdatePayload(filled, image).git_token).toBeUndefined();
  });

  it("omits a blank token rather than clearing the stored one", () => {
    // git_token is write-only and the field always renders empty, so blank
    // means "unchanged" here — the opposite of the source fields above.
    const payload = buildUpdatePayload({ ...filled, git_token: "" }, gitUrl);
    expect(payload.git_token).toBeUndefined();
  });

  it("sets the integration on the connection path", () => {
    expect(buildUpdatePayload(filled, gitConnection).git_integration_id).toBe(
      "int-1",
    );
  });

  it("clears the integration when a git app is edited onto a plain URL", () => {
    expect(buildUpdatePayload(filled, gitUrl).git_integration_id).toBe("");
  });

  it("omits the integration for an image app, preserving it", () => {
    // An image app may have been a git app; clearing it would lose the
    // connection on a switch back.
    expect(
      buildUpdatePayload(filled, image).git_integration_id,
    ).toBeUndefined();
  });
});

// The object literal as it was written inline in application-settings-form.tsx
// before the extraction, transcribed verbatim. Kept so the refactor can be
// shown to produce identical bodies rather than argued to.
function originalInline(
  value: SourceFormValues,
  isGit: boolean,
  gitSource: "connection" | "url",
  gitIntegrationId: string,
) {
  return {
    name: value.name || undefined,
    source_repo: isGit ? value.source_repo : "",
    source_image: isGit ? "" : value.source_image,
    dockerfile_path: isGit ? value.dockerfile_path : "",
    root_directory: isGit ? value.root_directory : "",
    branch: value.branch,
    build_type_override: isGit ? value.build_type_override : "",
    git_token:
      isGit && gitSource === "url" ? value.git_token || undefined : undefined,
    git_integration_id: isGit
      ? gitSource === "connection"
        ? gitIntegrationId
        : ""
      : undefined,
  };
}

describe("buildUpdatePayload is the inline literal it replaced", () => {
  it("produces an identical body for every blank/filled combination", () => {
    const fields = [
      "name",
      "source_repo",
      "source_image",
      "dockerfile_path",
      "root_directory",
      "build_type_override",
      "git_token",
      "branch",
    ] as const;

    let checked = 0;
    // Each field independently blank or filled: 2^8 shapes.
    for (let mask = 0; mask < 1 << fields.length; mask++) {
      const value = {} as SourceFormValues;
      fields.forEach((field, bit) => {
        value[field] = mask & (1 << bit) ? filled[field] : "";
      });

      for (const isGit of [true, false]) {
        for (const gitSource of ["connection", "url"] as const) {
          for (const gitIntegrationId of ["int-1", ""]) {
            expect(
              buildUpdatePayload(value, { isGit, gitSource, gitIntegrationId }),
            ).toEqual(
              originalInline(value, isGit, gitSource, gitIntegrationId),
            );
            checked++;
          }
        }
      }
    }

    // 256 shapes x 2 types x 2 git sources x 2 integration ids
    expect(checked).toBe(2048);
  });
});

describe("application registry credential intent", () => {
  it("omits a blank password and preserves the stored credential", () => {
    const data = buildUpdatePayload(
      { ...filled, registry_username: "app-user", registry_password: "" },
      image,
    );
    expect(data.registry_username).toBe("app-user");
    expect(data.registry_password).toBeUndefined();
  });
  it("sends explicit removal without sending a credential", () => {
    const data = buildUpdatePayload(
      {
        ...filled,
        registry_username: "app-user",
        registry_password: "secret",
        clear_registry_credentials: true,
      },
      image,
    );
    expect(data.clear_registry_credentials).toBe(true);
    expect(data.registry_username).toBeUndefined();
    expect(data.registry_password).toBeUndefined();
  });
  it("never sends image credentials on the git path", () => {
    const data = buildUpdatePayload(
      { ...filled, registry_username: "app-user", registry_password: "secret" },
      gitUrl,
    );
    expect(data.registry_username).toBeUndefined();
    expect(data.registry_password).toBeUndefined();
  });
});
