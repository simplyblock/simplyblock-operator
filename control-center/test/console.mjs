// The harness the browser tests share: serves this checkout over loopback and
// opens the console in the system Chrome through playwright-core, on the
// in-browser fixture backend (config.js has mock: true). React is answered
// from node_modules instead of unpkg, so the tests also run offline. It is a
// module of its own, not a *.test.mjs file, so `node --test` does not run it.
import http from "node:http";
import {createReadStream, existsSync, statSync} from "node:fs";
import path from "node:path";
import {fileURLToPath} from "node:url";
import {chromium} from "playwright-core";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

export async function startConsole() {
  const server = http.createServer((req, res) => {
    const p = path.join(root, path.normalize(decodeURIComponent(new URL(req.url, "http://x").pathname)));
    const file = existsSync(p) && statSync(p).isFile() ? p : path.join(root, "index.html");
    res.writeHead(200, {"Content-Type": file.endsWith(".js") ? "application/javascript" : "text/html"});
    createReadStream(file).pipe(res);
  });
  await new Promise(r => server.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${server.address().port}/`;
  const browser = await chromium.launch({channel: "chrome", headless: true});

  // open loads the console in a fresh browser context (empty localStorage) and
  // waits for the shell to render its breadcrumb.
  async function open(url = base) {
    const ctx = await browser.newContext();
    await ctx.route("https://unpkg.com/**", route => {
      const name = new URL(route.request().url()).pathname.split("/").pop();
      const pkg = name.startsWith("react-dom") ? "react-dom" : "react";
      route.fulfill({path: path.join(root, "node_modules", pkg, "umd", name), contentType: "application/javascript"});
    });
    const page = await ctx.newPage();
    const errors = [];
    page.on("pageerror", e => errors.push(e.message));
    await page.goto(url);
    await page.waitForSelector(".crumb.cur", {timeout: 20000}).catch(() => {
      throw new Error("the console did not render: " + errors.join("; "));
    });
    return page;
  }

  async function stop() {
    await browser.close();
    server.close();
  }
  return {base, open, stop};
}
