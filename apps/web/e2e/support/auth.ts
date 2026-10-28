import { fileURLToPath } from "node:url";

/**
 * Where the signed-in session is kept between the setup project and the specs.
 * Absolute, so it means the same thing to the config and to the setup test
 * whatever directory Playwright was started from. Git-ignored: it holds a live
 * session for the stack under test.
 */
export const AUTH_FILE = fileURLToPath(
  new URL("../.auth/admin.json", import.meta.url),
);
