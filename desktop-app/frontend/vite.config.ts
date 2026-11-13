/// <reference types="vitest/config" />
import { writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { defineConfig, type Plugin } from "vite";
import wails from "@wailsio/runtime/plugins/vite";

// The root .gitignore ignores dist/, but `//go:embed all:frontend/dist` in
// main.go needs the folder to exist, so a fresh clone can run `go vet` and
// `go test` before any frontend build. Git tracks dist/.gitkeep; Vite empties
// dist on every build, so this puts the file back.
function keepDistPlaceholder(): Plugin {
  let outDir = "";
  return {
    name: "homeport:keep-dist-placeholder",
    apply: "build",
    configResolved(config) {
      outDir = resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      writeFileSync(resolve(outDir, ".gitkeep"), "");
    },
  };
}

export default defineConfig({
  server: {
    host: "127.0.0.1",
    port: Number(process.env.WAILS_VITE_PORT) || 9245,
    strictPort: true,
  },
  plugins: [wails("./bindings"), keepDistPlaceholder()],
  test: {
    include: ["src/**/*.test.ts"],
  },
});
