import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Two entries: the application every browser loads (index.html) and the
// administrator extension (src/admin/entry.tsx). The extension is never
// referenced by the application: the server lists its URL only to
// administrators (GET /api/v1/ui/modules) and serves its files only to them,
// reading which files belong to it from the manifest (dist/.vite/manifest.json).
// Output names are content hashes only, so no file name tells what it holds.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    manifest: true,
    rolldownOptions: {
      input: {
        main: "index.html",
        admin: "src/admin/entry.tsx",
      },
      output: {
        entryFileNames: "assets/[hash].js",
        chunkFileNames: "assets/[hash].js",
        assetFileNames: "assets/[hash][extname]",
      },
    },
  },
});
