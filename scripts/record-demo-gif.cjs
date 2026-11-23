#!/usr/bin/env node
// Re-records docs/assets/demo.gif: drives the static demo console (web/dist-demo)
// in a headless browser (create a branch, show how to connect, save a checkpoint,
// reset) and turns the video into a looping GIF with ffmpeg.
//
//   make demo                                  # build web/dist-demo first
//   npm i --prefix /tmp/pw playwright@1.58.2   # once; any directory will do
//   NODE_PATH=/tmp/pw/node_modules node scripts/record-demo-gif.cjs [out.gif]
//
// Needs ffmpeg on PATH and a Playwright Chromium (npx playwright install chromium).
// Env: DEMO_URL (use a running demo instead of serving web/dist-demo),
//      GIF_WIDTH (default 1200), GIF_FPS (default 12), CHROMIUM_CHANNEL (default chromium).
const http = require("node:http");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { execFileSync } = require("node:child_process");
const { chromium } = require("playwright");

const root = path.resolve(__dirname, "..");
const dist = path.join(root, "web", "dist-demo");
const out = path.resolve(process.argv[2] || path.join(root, "docs", "assets", "demo.gif"));
const width = Number(process.env.GIF_WIDTH || 1200);
const fps = Number(process.env.GIF_FPS || 10);
const view = { width: 1200, height: 760 };

const types = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".txt": "text/plain", ".xml": "application/xml" };

// serve: web/dist-demo as a static site that answers any other path with index.html.
function serve() {
  const server = http.createServer((req, res) => {
    const file = path.join(dist, decodeURIComponent(new URL(req.url, "http://x").pathname));
    const real = file.startsWith(dist) && fs.existsSync(file) && fs.statSync(file).isFile() ? file : path.join(dist, "index.html");
    res.setHeader("content-type", types[path.extname(real)] || "application/octet-stream");
    res.end(fs.readFileSync(real));
  });
  return new Promise((resolve) => server.listen(0, "127.0.0.1", () => resolve(server)));
}

// A visible pointer: a video has no mouse cursor.
const POINTER = `
  const dot = document.createElement("div");
  dot.style.cssText = "position:fixed;z-index:99999;width:18px;height:18px;margin:-9px 0 0 -9px;border-radius:50%;background:rgba(15,118,110,.35);border:2px solid #0f766e;pointer-events:none;transition:transform .08s;left:-50px;top:-50px";
  addEventListener("DOMContentLoaded", () => document.body.appendChild(dot));
  addEventListener("mousemove", (e) => { dot.style.left = e.clientX + "px"; dot.style.top = e.clientY + "px"; }, true);
  addEventListener("mousedown", () => (dot.style.transform = "scale(.7)"), true);
  addEventListener("mouseup", () => (dot.style.transform = ""), true);
`;

async function main() {
  const server = process.env.DEMO_URL ? null : await serve();
  const url = process.env.DEMO_URL || `http://127.0.0.1:${server.address().port}/`;
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "demo-gif-"));
  const browser = await chromium.launch({ channel: process.env.CHROMIUM_CHANNEL || "chromium" });
  const ctx = await browser.newContext({ viewport: view, recordVideo: { dir: tmp, size: view }, locale: "en-US" });
  await ctx.addInitScript(POINTER);
  const page = await ctx.newPage();
  const pause = (ms) => page.waitForTimeout(ms);
  const click = async (locator) => {
    const box = await locator.boundingBox();
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 12 });
    await pause(250);
    await locator.click();
  };

  await page.goto(url);
  await page.getByText("Skip tour").click();
  await pause(900);

  // 1. Create a branch from the "orders" base.
  const name = page.locator("input.mono").first();
  await name.fill("");
  await name.pressSequentially("try-it", { delay: 90 });
  await pause(500);
  await click(page.getByRole("button", { name: "Create branch" }));
  const row = page.locator(".brow").filter({ hasText: "try-it" });
  await row.waitFor();
  await page.getByText("Your Branch is ready").waitFor({ timeout: 15000 });
  await pause(600);

  // 2. How to connect.
  await page.getByText("Your Branch is ready").scrollIntoViewIfNeeded();
  await page.evaluate(() => window.scrollBy(0, 40));
  await pause(700);
  await click(page.getByRole("button", { name: "Show password" }));
  await pause(1100);
  await click(page.getByRole("tab", { name: "Command" }));
  await pause(1500);

  // 3. Save a checkpoint before risky work.
  const cp = page.getByPlaceholder("Name (optional)");
  await cp.scrollIntoViewIfNeeded();
  await click(cp);
  await cp.pressSequentially("before-migration", { delay: 70 });
  await click(page.getByRole("button", { name: "Save checkpoint" }));
  await pause(1800);

  // 4. Reset the branch.
  await click(page.getByRole("button", { name: "Reset", exact: true }));
  await pause(900);
  await click(page.getByRole("button", { name: "Confirm reset" }));
  await pause(3600);

  const video = page.video();
  await ctx.close();
  await browser.close();
  if (server) server.close();
  const webm = await video.path();

  // webm -> GIF with a palette made from the video itself; loops forever.
  const filters = `fps=${fps},scale=${width}:-1:flags=lanczos`;
  fs.mkdirSync(path.dirname(out), { recursive: true });
  execFileSync("ffmpeg", ["-y", "-loglevel", "error", "-i", webm, "-vf", `${filters},split[a][b];[a]palettegen=max_colors=96:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle`, "-loop", "0", out]);
  fs.rmSync(tmp, { recursive: true, force: true });
  console.log(`${out}: ${(fs.statSync(out).size / 1024 / 1024).toFixed(2)} MB`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
