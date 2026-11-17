import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";
import { defineConfig, globalIgnores } from "eslint/config";

export default defineConfig([
  globalIgnores(["dist"]),
  {
    // HTTP installs cannot use secure-context APIs directly. Keep access in
    // helpers so localhost development cannot hide another production failure.
    files: ["**/*.{ts,tsx}"],
    rules: {
      "no-restricted-syntax": [
        "error",
        {
          selector:
            'MemberExpression[object.name="crypto"][property.name="randomUUID"]',
          message:
            "crypto.randomUUID is secure-context only; use newClientId() from @/lib/utils/client-id.",
        },
      ],
    },
  },
  {
    files: ["**/*.{ts,tsx}"],
    ignores: ["src/lib/utils/clipboard.ts"],
    rules: {
      "no-restricted-syntax": [
        "error",
        {
          selector:
            'MemberExpression[object.name="crypto"][property.name="randomUUID"]',
          message:
            "crypto.randomUUID is secure-context only; use newClientId() from @/lib/utils/client-id.",
        },
        {
          selector:
            'MemberExpression[object.name="navigator"][property.name="clipboard"]',
          message:
            "navigator.clipboard is secure-context only; use copyToClipboard() from @/lib/utils/clipboard.",
        },
      ],
    },
  },
  {
    files: ["**/*.{ts,tsx}"],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      ecmaVersion: 2020,
      globals: globals.browser,
    },
  },
  {
    // Route modules and shadcn UI primitives legitimately export more than a
    // single component — TanStack Router's `Route`, and the `*Variants` helpers
    // shadcn generates alongside each component. only-export-components is a
    // Fast Refresh (dev HMR) convenience rule with no runtime meaning, so it is
    // off here rather than worked around.
    files: ["src/routes/**/*.{ts,tsx}", "src/components/ui/**/*.{ts,tsx}"],
    rules: {
      "react-refresh/only-export-components": "off",
    },
  },
  {
    // The Playwright drills run in node, not the browser: they read process.env
    // and import from @playwright/test. Their page.evaluate callbacks are
    // browser code, so both global sets apply rather than either one.
    files: ["e2e/**/*.ts", "playwright.config.ts"],
    languageOptions: {
      globals: { ...globals.browser, ...globals.node },
    },
  },
  {
    // eslint-plugin-react-hooks v7 folded the React Compiler's ruleset in as
    // errors. Belune does not use the React Compiler, so these are not blocking
    // requirements — but they are genuine "rules of React" (a pure render, refs
    // untouched during render, no cascading setState from an effect), so they
    // stay as warnings to guard new code rather than being switched off. Clear
    // the existing baseline in a dedicated pass, not piecemeal.
    files: ["**/*.{ts,tsx}"],
    rules: {
      "react-hooks/set-state-in-effect": "warn",
      "react-hooks/purity": "warn",
      "react-hooks/refs": "warn",
    },
  },
]);
