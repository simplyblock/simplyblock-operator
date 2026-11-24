// Browser-history behavior of the console shell: Back and Forward move between
// views, and a view's URL opens that view in a fresh browser. It drives the
// system Chrome against the checked-in page and its in-browser fixture
// backend, through the shared harness in console.mjs.
//
// Run: npm test   (or: node --test test/)
import {test, before, after} from "node:test";
import assert from "node:assert/strict";
import {startConsole} from "./console.mjs";

let console_, base, open;

before(async () => {
  console_ = await startConsole();
  ({base, open} = console_);
});

after(async () => { await console_.stop(); });

// crumbs is the breadcrumb as the user reads it, the shell's location.
const crumbs = page => page.$$eval(".crumbs .crumb", els => els.map(e => e.textContent.trim()).join(" > "));

// navigate moves through the shell's own navigation, as a click would.
async function navigate(page, call, arg) {
  const before = await crumbs(page);
  await page.evaluate(call, arg);
  await page.waitForFunction(b => [...document.querySelectorAll(".crumbs .crumb")].map(e => e.textContent.trim()).join(" > ") !== b, before);
  return crumbs(page);
}

// shows waits briefly for the breadcrumb to read `want`, then reports where the
// browser actually is: a Back that left the console has no breadcrumb at all.
async function shows(page, want) {
  await page.waitForFunction(w => [...document.querySelectorAll(".crumbs .crumb")].map(e => e.textContent.trim()).join(" > ") === w, want, {timeout: 5000}).catch(() => {});
  const url = page.url();
  return url.startsWith(base) ? await crumbs(page) : `left the console for ${url}`;
}

test("Back and Forward move between the views the user visited", async () => {
  const page = await open(base);
  const home = await crumbs(page);
  const access = await navigate(page, () => window.__nav.access());
  const k8s = await navigate(page, () => window.__nav.k8s());
  assert.notEqual(access, home);
  assert.notEqual(k8s, access);

  await page.goBack();
  assert.equal(await shows(page, access), access, "Back returns to the previous view");
  await page.goBack();
  assert.equal(await shows(page, home), home, "a second Back returns to the first view");
  await page.goForward();
  assert.equal(await shows(page, access), access, "Forward replays the next view");
  await page.context().close();
});

test("a deep view's URL, ids and parameters included, opens that view in a fresh browser", async () => {
  const page = await open(base);
  await navigate(page, () => window.__nav.k8s());
  // an object key carries every separator the URL encoding uses
  const deep = await navigate(page, () => window.__nav.openCR({ctx: "local", ns: "simplyblock", kind: "StorageCluster", name: "a/b c=d;e"}));
  const deepUrl = page.url();
  await navigate(page, () => window.__nav.alerts({severity: "warning"}));
  const alertsUrl = page.url();
  await page.context().close();

  const fresh = await open(deepUrl);
  assert.equal(await crumbs(fresh), deep, `opening ${deepUrl} shows the object it was copied from`);
  await fresh.context().close();
  const withParams = await open(alertsUrl);
  assert.equal(withParams.url(), alertsUrl, "a view's parameters survive the round trip through its URL");
  await withParams.context().close();
});

test("Back from another view returns to the log explorer on the same query", async () => {
  const page = await open(base);
  const home = await crumbs(page);
  const logs = await navigate(page, () => window.__nav.logs({q: "timeout"}));
  const logsUrl = page.url();
  await navigate(page, () => window.__nav.k8s());

  await page.goBack();
  assert.equal(await shows(page, logs), logs, "Back returns to the log explorer");
  assert.equal(page.url(), logsUrl, "the explorer's entry still carries its query");
  await page.goBack();
  assert.equal(await shows(page, home), home, "the view before the explorer keeps its own entry");
  await page.context().close();
});

test("a view's URL opens that view in a fresh browser", async () => {
  const page = await open(base);
  const target = await navigate(page, () => window.__nav.k8s());
  const url = page.url();
  await page.context().close();

  const fresh = await open(url);
  assert.equal(await crumbs(fresh), target, `opening ${url} shows the view it was copied from`);
  await fresh.context().close();
});
