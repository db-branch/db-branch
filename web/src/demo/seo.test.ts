import { describe, expect, it } from "vitest";
import { DEMO_ROBOTS_TXT, DEMO_SITEMAP_XML, transformDemoHtml } from "./seo";

const BASE_HTML = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>db-branch</title>
  </head>
  <body>
    <div id="root"></div>
  </body>
</html>`;

describe("demo SEO helpers", () => {
  it("injects canonical link, title, description, robots meta, and analytics beacon", () => {
    const out = transformDemoHtml(BASE_HTML);
    expect(out).toContain('<title>db-branch demo: database branches in your browser</title>');
    expect(out).toContain('<link rel="canonical" href="https://demo.db-branch.com/" />');
    expect(out).toContain('<meta name="robots" content="index,follow" />');
    expect(out).toContain('<meta name="description" content="');
    expect(out).toContain("https://static.cloudflareinsights.com/beacon.min.js");
  });

  it("provides a valid robots.txt pointing to the demo sitemap", () => {
    expect(DEMO_ROBOTS_TXT).toContain("User-agent: *");
    expect(DEMO_ROBOTS_TXT).toContain("Allow: /");
    expect(DEMO_ROBOTS_TXT).toContain("Sitemap: https://demo.db-branch.com/sitemap.xml");
  });

  it("provides a valid sitemap.xml listing only the canonical demo root URL", () => {
    expect(DEMO_SITEMAP_XML).toContain('<?xml version="1.0" encoding="UTF-8"?>');
    expect(DEMO_SITEMAP_XML).toContain('<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">');
    expect(DEMO_SITEMAP_XML).toContain("<loc>https://demo.db-branch.com/</loc>");
    expect(DEMO_SITEMAP_XML.match(/<loc>/g)).toHaveLength(1);
  });
});
