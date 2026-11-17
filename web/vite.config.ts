import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The Go server serves the built files under /ui/ and the index page on the UI's own paths. The
// dev server proxies the API to a local Outpost, so `npm run dev` works against `outpost serve`.
export default defineConfig({
  base: "/ui/",
  plugins: [react()],
  build: {
    outDir: "../internal/api/web/app",
    emptyOutDir: true,
    modulePreload: { polyfill: false },
  },
  server: { proxy: { "/api": "http://localhost:7400", "/login": "http://localhost:7400" } },
});
