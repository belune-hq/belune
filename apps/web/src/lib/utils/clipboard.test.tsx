import { afterEach, expect, it, vi } from "vitest";
import { copyToClipboard } from "./clipboard";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  document.body.replaceChildren();
});

it("uses the Clipboard API when available", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  expect(await copyToClipboard("secret")).toBe(true);
  expect(writeText).toHaveBeenCalledWith("secret");
});

it("reports Clipboard API rejection", async () => {
  vi.stubGlobal("navigator", {
    clipboard: { writeText: vi.fn().mockRejectedValue(new Error("denied")) },
  });
  expect(await copyToClipboard("secret")).toBe(false);
});

it.each([true, false, "throw"])(
  "falls back without clipboard, restores selection and cleans up (%s)",
  async (result) => {
    vi.stubGlobal("navigator", {});
    const input = document.createElement("input");
    input.value = "previous selection";
    document.body.appendChild(input);
    input.focus();
    input.setSelectionRange(1, 5);
    const command = vi.fn((name: string) => {
      expect(name).toBe("copy");
      const textarea = document.querySelector("textarea");
      expect(textarea?.value).toBe("copied text");
      if (result === "throw") throw new Error("blocked");
      return result;
    });
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: command,
    });
    expect(await copyToClipboard("copied text")).toBe(result === true);
    expect(command).toHaveBeenCalledOnce();
    expect(document.querySelector("textarea")).toBeNull();
    expect(document.activeElement).toBe(input);
    expect(input.selectionStart).toBe(1);
    expect(input.selectionEnd).toBe(5);
    Reflect.deleteProperty(document, "execCommand");
  },
);
