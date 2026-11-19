import { readFileSync } from "node:fs";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import { DEMO_ROBOTS_TXT, DEMO_SITEMAP_XML, transformDemoHtml } from "./src/demo/seo.ts";

// The static demo (npm run build:demo = vite build --mode demo) is the same console with a fake agent
// in the browser (src/demo). Its page gets a title, canonical link, description, robots meta and the
// Cloudflare Web Analytics beacon; its headers file comes from demo-worker/, plus robots.txt and sitemap.xml.
function demoSite(): Plugin {
  return {
    name: "db-branch-demo-site",
    transformIndexHtml(html) {
      return transformDemoHtml(html);
    },
    generateBundle() {
      this.emitFile({ type: "asset", fileName: "_headers", source: readFileSync(new URL("./demo-worker/_headers", import.meta.url), "utf8") });
      this.emitFile({ type: "asset", fileName: "robots.txt", source: DEMO_ROBOTS_TXT });
      this.emitFile({ type: "asset", fileName: "sitemap.xml", source: DEMO_SITEMAP_XML });
    },
  };
}

// The console is served by the agent; `npm run dev` proxies the API to a dev VM.
export default defineConfig(({ mode }) => ({
  plugins: [react(), ...(mode === "demo" ? [demoSite()] : [])],
  build: { outDir: mode === "demo" ? "dist-demo" : "dist", emptyOutDir: true, assetsDir: "assets" },
  server: {
    proxy: { "/v1": { target: process.env.DBB_AGENT ?? "https://127.0.0.1:7080", secure: false } },
  },
}));
