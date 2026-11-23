import { cloudflareTest } from "@cloudflare/vitest-pool-workers";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.toml" },
      miniflare: {
        // The pool-workers workerd build stops at 2026-08-22. Wrangler itself
        // is newer; this override is test-only.
        compatibilityDate: "2026-08-22",
        bindings: {
          ADMIN_TOKEN: "test-admin-secret",
        },
      },
    }),
  ],
  test: {
    testTimeout: 30_000,
    hookTimeout: 60_000,
  },
});
