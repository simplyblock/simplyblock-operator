#!/usr/bin/env node
// Prepares the design document for a container image:
//
//   1. strips the fixture backend (everything between MOCK:START / MOCK:END),
//      so no mock data ships to a cluster
//   2. repoints the React and webfont tags at vendored copies, so the
//      pod needs no network egress, and swaps the development React builds for
//      the production ones
//   3. copies only the files the page actually references
//
// Usage: node deploy/build/prepare.mjs [--keep-mocks=0|1] --out /out
import {readFile, writeFile, mkdir, cp} from "node:fs/promises";
import {existsSync} from "node:fs";
import path from "node:path";

const args = Object.fromEntries(process.argv.slice(2).map(a => {
  const [k, v] = a.replace(/^--/, "").split("=");
  return [k, v === undefined ? true : v];
}));
const out = args.out || "/out";
const keepMocks = args["keep-mocks"] === "1" || args["keep-mocks"] === true;
const root = process.cwd();

let html = await readFile(path.join(root, "index.html"), "utf8");

// 1. the fixture backend
if (!keepMocks) {
  const before = html.length;
  html = html.replace(/<!-- MOCK:START[\s\S]*?<!-- MOCK:END -->\n?/g, "");
  if (html.length === before) throw new Error("MOCK:START / MOCK:END markers not found in index.html");
  if (/mock-[a-z-]+\.jsx|dist\/mock\.js/.test(html)) throw new Error("a mock script survived the strip: " + html.match(/mock-[a-z-]+\.jsx|dist\/mock\.js/g));
  // The JSX is precompiled into dist/app.js (build.md). A page that still
  // transpiles in the browser needs unsafe-eval, which the CSP no longer grants.
  if (/text\/babel|babel\.min\.js/.test(html)) throw new Error("index.html still transpiles in the browser — run the dist build (build.md) first");
  if (!/dist\/app\.js/.test(html)) throw new Error("index.html does not load dist/app.js");
  console.log(`stripped the fixture backend (${before - html.length} bytes)`);
}

// 2. vendored dependencies, production builds
const VENDOR = [
  [/<script src="https:\/\/unpkg\.com\/react@[^"]+"[^>]*><\/script>/,
    '<script src="vendor/react.production.min.js"></script>'],
  [/<script src="https:\/\/unpkg\.com\/react-dom@[^"]+"[^>]*><\/script>/,
    '<script src="vendor/react-dom.production.min.js"></script>']
];
for (const [re, rep] of VENDOR) {
  if (!re.test(html)) throw new Error("could not find the CDN tag for " + rep);
  html = html.replace(re, rep);
}
if (/unpkg\.com/.test(html)) throw new Error("a CDN reference survived: " + html.match(/https:\/\/unpkg[^"]+/g));

// 2b. self-hosted typefaces.
// The mono carries every UUID and CRD field name in the console, so losing it
// to a blocked CDN is not a cosmetic regression. The two preconnects go away
// with the stylesheet they were warming up.
const FONT_LINK = /<link[^>]+fonts\.googleapis\.com\/css2[^>]*>/;
if (!FONT_LINK.test(html)) throw new Error("could not find the Google Fonts stylesheet link");
html = html.replace(FONT_LINK, '<link rel="stylesheet" href="vendor/fonts.css">');
html = html.replace(/[ \t]*<link[^>]+rel="preconnect"[^>]+fonts\.g(?:oogleapis|static)\.com[^>]*>\n?/g, "");
if (/fonts\.(googleapis|gstatic)\.com/.test(html))
  throw new Error("a font CDN reference survived: " + html.match(/[^"']*fonts\.g[^"']*/g));

// 3. only what the page loads
const referenced = [...html.matchAll(/(?:src|href)="([^":]+\.(?:jsx|js|css))"/g)].map(m => m[1])
  .filter(f => !f.startsWith("vendor/"));
const files = [...new Set(referenced)];
// config.js is written at container start, so it must not come from the repo
const shipped = files.filter(f => f !== "config.js");
for (const f of shipped) {
  if (!existsSync(path.join(root, f))) throw new Error(`index.html references ${f}, which does not exist`);
}

await mkdir(out, {recursive: true});
await writeFile(path.join(out, "index.html"), html);
for (const f of shipped) {
  await mkdir(path.dirname(path.join(out, f)), {recursive: true});
  await cp(path.join(root, f), path.join(out, f));
}

// 4. no subresource may reach off-cluster.
//
// The CSP is 'self' and the pod has no route to the public internet, so an
// external subresource is a silently broken page rather than a slow one. This
// checks every shipped file, not just index.html — the brand mark lives in
// app.jsx, which is exactly how it was missed before.
//
// Scoped to SUBRESOURCES: src=, and href= on <link>. A plain <a href> is a
// navigation target — CSP does not touch it and it needs no egress — so an
// outbound link in the footer must not fail the build.
const SUBRESOURCE = [
  /\bsrc=["'](https?:\/\/[^"']+)["']/g,
  /<link\b[^>]*\bhref=["'](https?:\/\/[^"']+)["']/g
];
const offenders = [];
for (const f of ["index.html", ...shipped]) {
  const body = f === "index.html" ? html : await readFile(path.join(root, f), "utf8");
  for (const re of SUBRESOURCE) {
    for (const m of body.matchAll(re)) {
      // A literal CDN default sitting beside an SB_CONFIG lookup is dead in
      // the container, since the generated config always supplies the value.
      // Nothing matches this today — the logo reads its URL through a JSX
      // expression — but it keeps the guard honest if a default is ever
      // written as a plain attribute. Anything else is a real fetch.
      if (/SB_CONFIG/.test(body.slice(Math.max(0, m.index - 120), m.index))) continue;
      offenders.push(`${f}: ${m[1]}`);
    }
  }
}
if (offenders.length) throw new Error("external subresource(s) would be fetched at runtime:\n  " + offenders.join("\n  "));

console.log(`wrote index.html + ${shipped.length} asset(s) to ${out}`);
console.log(shipped.join(", "));
