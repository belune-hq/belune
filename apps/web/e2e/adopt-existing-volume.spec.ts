import { execFileSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";
import { settled } from "./support/settled";

async function api<T>(
  page: Page,
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  return page.evaluate(
    async ({ method, path, body }) => {
      const csrf = document.cookie.match(/(?:^|;\s*)csrf_token=([^;]+)/)?.[1];
      const response = await fetch(`/api${path}`, {
        method,
        headers: {
          "Content-Type": "application/json",
          ...(csrf ? { "X-CSRF-Token": csrf } : {}),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!response.ok)
        throw new Error(`${method} ${path}: ${response.status}`);
      return response.status === 204 ? null : response.json();
    },
    { method, path, body },
  ) as Promise<T>;
}

// Only this test's explicitly named volume is created/removed on the outer
// daemon. Host-wide prune tests live in the isolated Docker-in-Docker drill.
test("adoption and deletion explain and preserve existing Docker data", async ({
  page,
}, testInfo) => {
  const stamp = Date.now();
  const external = `adopt-browser-${stamp}`;
  execFileSync("docker", ["volume", "create", external]);
  let projectID: string | undefined;
  try {
    execFileSync("docker", [
      "run",
      "--rm",
      "--network=none",
      "-v",
      `${external}:/data`,
      "busybox:1.37.0",
      "sh",
      "-c",
      "printf 'browser-adopted-data\\n' > /data/marker",
    ]);
    await page.goto("/projects");
    const p = await api<{ id: string }>(page, "POST", "/projects", {
      name: `Adopt ${stamp}`,
      slug: `adopt-${stamp}`,
    });
    projectID = p.id;
    const app = await api<{ id: string }>(
      page,
      "POST",
      `/projects/${p.id}/applications`,
      {
        name: "Adopt Data",
        type: "image",
        build_type: "image",
        source_image: "busybox:1.37.0",
      },
    );
    const path = `/projects/${p.id}/applications/${app.id}`;
    await api(page, "POST", `${path}/volumes`, {
      name: "managed-data",
      mount_path: "/managed",
    });
    await page.goto(`${path}/mounts`);
    await page
      .getByRole("button", { name: "Adopt Existing Volume", exact: true })
      .click();
    const dialog = page.getByRole("dialog", {
      name: "Adopt Existing Volume",
      exact: true,
    });
    await expect(dialog).toContainText("retained");
    await dialog
      .getByLabel("Docker volume name", { exact: true })
      .fill(`${external}-missing`);
    await dialog
      .getByLabel("Mount name", { exact: true })
      .fill("external-data");
    await dialog.getByLabel("Mount path", { exact: true }).fill("/external");
    await page.screenshot({ path: testInfo.outputPath("adopt-dialog.png") });
    await dialog
      .getByRole("button", { name: "Adopt volume", exact: true })
      .click();
    const missing = page
      .locator("[data-sonner-toast]")
      .filter({ hasText: "does not exist" });
    await expect(missing).toBeVisible();
    expect((await settled(missing)).opacity).toBe(1);
    await dialog
      .getByLabel("Docker volume name", { exact: true })
      .fill(external);
    await dialog
      .getByRole("button", { name: "Adopt volume", exact: true })
      .click();
    await expect(dialog).not.toBeVisible();
    await expect(page.getByText("Adopted", { exact: true })).toBeVisible();
    const row = (
      await api<{ id: string; external_name?: string }[]>(
        page,
        "GET",
        `${path}/volumes`,
      )
    ).find((v) => v.external_name === external);
    expect(row).toBeDefined();
    await page.goto(`${path}/settings`);
    await page
      .getByRole("button", { name: "Delete application", exact: true })
      .click();
    const deletion = page.getByRole("alertdialog");
    await expect(deletion).toContainText(
      "1 managed volume will be removed: managed-data",
    );
    await expect(deletion).toContainText(
      `1 adopted volume will be left behind and remain protected from cleanup: ${external}`,
    );
    expect((await settled(deletion)).opacity).toBe(1);
    await page.screenshot({
      path: testInfo.outputPath("adopt-delete-warning.png"),
    });
    await deletion.getByRole("textbox").fill("Adopt Data");
    await deletion.getByRole("button", { name: "Delete", exact: true }).click();
    await expect(page).toHaveURL(`/projects/${p.id}`);
    const data = execFileSync(
      "docker",
      [
        "run",
        "--rm",
        "--network=none",
        "-v",
        `${external}:/data`,
        "busybox:1.37.0",
        "cat",
        "/data/marker",
      ],
      { encoding: "utf8" },
    );
    expect(data).toBe("browser-adopted-data\n");
    await page.goto("/docker?tab=volumes");
    await page.getByPlaceholder("Search by name or driver…").fill(external);
    await expect(
      page.getByText("Adopted, no application", { exact: true }),
    ).toBeVisible();
    const removal = page.getByText(`docker volume rm ${external}`, {
      exact: true,
    });
    await expect(removal).toBeVisible();
    const commandFits = await removal.evaluate((el) => {
      const box = el.getBoundingClientRect();
      return box.left >= 0 && box.right <= window.innerWidth;
    });
    expect(
      commandFits,
      "host removal instructions must fit in the viewport",
    ).toBe(true);
    await expect(page.getByRole("button", { name: /Release/ })).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("adopt-retained.png") });
  } finally {
    if (projectID) await api(page, "DELETE", `/projects/${projectID}`);
    execFileSync("docker", ["volume", "rm", external]);
  }
});
