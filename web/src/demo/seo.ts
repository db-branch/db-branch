const DEMO_ORIGIN = "https://demo.db-branch.com";
const DEMO_CANONICAL_URL = `${DEMO_ORIGIN}/`;
const DEMO_TITLE = "db-branch demo: database branches in your browser";
const DEMO_DESCRIPTION =
  "Try the db-branch web console with sample data, right in your browser. Nothing here is real: there is no server and no database behind it.";
const BEACON = `<script defer src="https://static.cloudflareinsights.com/beacon.min.js" data-cf-beacon='{"token": "ce9d171b3cfe4083963bb3b1813a8fed"}'></script>`;

/** Transforms the console index.html for the static demo site at demo.db-branch.com. */
export function transformDemoHtml(html: string): string {
  return html
    .replace("<title>db-branch</title>", `<title>${DEMO_TITLE}</title>`)
    .replace(
      "</head>",
      `    <link rel="canonical" href="${DEMO_CANONICAL_URL}" />\n` +
        `    <meta name="description" content="${DEMO_DESCRIPTION}" />\n` +
        `    <meta name="robots" content="index,follow" />\n  </head>`,
    )
    .replace("</body>", `    ${BEACON}\n  </body>`);
}

export const DEMO_ROBOTS_TXT = [
  "User-agent: *",
  "Allow: /",
  "",
  `Sitemap: ${DEMO_ORIGIN}/sitemap.xml`,
  "",
].join("\n");

export const DEMO_SITEMAP_XML = [
  '<?xml version="1.0" encoding="UTF-8"?>',
  '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">',
  `  <url><loc>${DEMO_CANONICAL_URL}</loc></url>`,
  "</urlset>",
  "",
].join("\n");
