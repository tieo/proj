import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The editor is served by the model server, which also answers /api/boards.
export default defineConfig({
  plugins: [react()],
  base: "./",
  define: { "process.env.IS_PREACT": JSON.stringify("false") },
  build: { outDir: "dist", emptyOutDir: true },
  server: { proxy: { "/api": "http://127.0.0.1:8099" } },
});
