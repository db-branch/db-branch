import { readFileSync } from "node:fs";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

// The static demo (npm run build:demo = vite build --mode demo) is the same console with a fake agent
// in the browser (src/demo). Its page gets a title, a description, the robots meta and the Cloudflare
// Web Analytics beacon; its headers file comes from demo-worker/.
const BEACON = `<script defer src="https://static.cloudflareinsights.com/beacon.min.js" data-cf-beacon='{"token": "ce9d171b3cfe4083963bb3b1813a8fed"}'></script>`;

function demoSite(): Plugin {
  return {
    name: "db-branch-demo-site",
    transformIndexHtml(html) {
      return html
        .replace("<title>db-branch</title>", "<title>db-branch demo: database branches in your browser</title>")
        .replace(
          "</head>",
          `<meta name="description" content="Try the db-branch web console with sample data, right in your browser. Nothing here is real: there is no server and no database behind it." />\n` +
            `    <meta name="robots" content="index,follow" />\n  </head>`,
        )
        .replace("</body>", `    ${BEACON}\n  </body>`);
    },
    generateBundle() {
      this.emitFile({ type: "asset", fileName: "_headers", source: readFileSync(new URL("./demo-worker/_headers", import.meta.url), "utf8") });
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
