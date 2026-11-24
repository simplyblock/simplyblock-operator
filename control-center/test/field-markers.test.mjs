// The CR view honors the two field markers the operator's CRDs carry as
// annotations (operator/hack/fieldmarkers): a field marked operatorManaged is
// badged and locked like an immutable one, and a field marked invisible is
// not shown. The CRD and the object are stubbed through the console's own
// Kubernetes client, so the test needs neither a cluster nor a regenerated CRD.
//
// Run: npm test
import {test, before, after} from "node:test";
import assert from "node:assert/strict";
import {startConsole} from "./console.mjs";

let console_;
before(async () => { console_ = await startConsole(); });
after(async () => { await console_.stop(); });

// openFixture opens the CR view of a StorageCluster the stubbed client serves,
// next to a CRD whose annotations mark spec.vcpuCount operator-managed and
// spec.maxSubsystemCount and spec.secrets invisible.
async function openFixture() {
  const page = await console_.open();
  await page.evaluate(() => {
    const version = (RESOURCES.StorageCluster || {}).version || VERSION;
    const crd = {
      apiVersion: "apiextensions.k8s.io/v1", kind: "CustomResourceDefinition",
      metadata: {name: "storageclusters." + GROUP, annotations: {
        "simplyblock.io/operator-managed-fields": JSON.stringify({[version]: ["spec.vcpuCount"]}),
        "simplyblock.io/invisible-fields": JSON.stringify({[version]: ["spec.maxSubsystemCount", "spec.secrets"]}),
      }},
      spec: {group: GROUP, names: {kind: "StorageCluster", plural: "storageclusters"}, versions: [{name: version, served: true, storage: true, schema: {openAPIV3Schema: {
        type: "object", properties: {spec: {type: "object", properties: {
          vcpuCount: {type: "integer", description: "vCPUs per node."},
          maxSubsystemCount: {type: "integer"},
          minHugePagesSize: {type: "string"},
          secrets: {type: "object", properties: {token: {type: "string"}}},
        }}},
      }}}]},
    };
    const obj = {
      apiVersion: GROUP + "/" + version, kind: "StorageCluster",
      metadata: {name: "fixture", namespace: "sb-fixture", uid: "fixture-uid", generation: 1},
      spec: {vcpuCount: 4, maxSubsystemCount: 128, minHugePagesSize: "2Mi", secrets: {token: "hidden-value"}},
      status: {observedGeneration: 1},
    };
    const get = window.k8s.get;
    window.k8s.get = (kind, name, opts) => kind === "CustomResourceDefinition" && name === crd.metadata.name ? Promise.resolve(crd)
      : kind === "StorageCluster" && name === "fixture" ? Promise.resolve(JSON.parse(JSON.stringify(obj)))
      : get.call(window.k8s, kind, name, opts);
    window.__nav.openCR({ctx: "local", ns: "sb-fixture", kind: "StorageCluster", name: "fixture"});
  });
  await page.waitForSelector(".crpanel");
  // the CRD is read asynchronously; its description reaching the row is the sign it arrived
  await page.waitForFunction(() => [...document.querySelectorAll(".crpanel [title]")].some(e => e.title === "vCPUs per node."), null, {timeout: 10000});
  return page;
}

// essentialsRow is the Essentials row labeled `label` (a <dt>, its value in the
// next <dd>): the badges on the value and whether it offers an edit, or null
// when there is no such row.
const essentialsRow = (page, label) => page.evaluate(l => {
  const dt = [...document.querySelectorAll(".crpanel dl.props > dt")].find(e => e.textContent.trim() === l);
  if (!dt) return null;
  const dd = dt.nextElementSibling;
  return {badges: [...dd.querySelectorAll(".immut")].map(b => b.textContent.trim()), editable: !!dd.querySelector(".pen")};
}, label);

test("an operator-managed field is badged and cannot be edited in Essentials", async () => {
  const page = await openFixture();
  const managed = await essentialsRow(page, "vcpuCount");
  assert.ok(managed, "the vcpuCount row is shown");
  assert.deepEqual(managed.badges, ["operator-managed"]);
  assert.equal(managed.editable, false, "an operator-managed field offers no edit");
  const plain = await essentialsRow(page, "minHugePagesSize");
  assert.equal(plain.editable, true, "an unmarked spec field stays editable");
  await page.context().close();
});

test("an invisible field is not shown in Essentials", async () => {
  const page = await openFixture();
  assert.equal(await essentialsRow(page, "maxSubsystemCount"), null);
  await page.context().close();
});

test("the Everything tree badges operator-managed fields and leaves out invisible ones", async () => {
  const page = await openFixture();
  await page.click(".crpanel .tab:has-text('Everything')");
  await page.waitForSelector(".crpanel .jt");
  const keys = await page.$$eval(".crpanel .card .jt .k", els => els.map(e => ({k: e.textContent.trim(), badge: (e.nextElementSibling && e.nextElementSibling.classList.contains("immut")) ? e.nextElementSibling.textContent.trim() : null})));
  const byKey = Object.fromEntries(keys.map(x => [x.k, x.badge]));
  assert.equal(byKey.vcpuCount, "operator-managed");
  assert.ok(!("maxSubsystemCount" in byKey), "maxSubsystemCount is left out of the tree");
  assert.ok(!("secrets" in byKey) && !("token" in byKey), "an invisible object is left out with everything below it");
  assert.ok("minHugePagesSize" in byKey, "an unmarked field is still in the tree");
  await page.context().close();
});
