// The CR view honors the field markers the operator's CRDs carry as
// annotations (operator/hack/fieldmarkers): a field marked operatorManaged is
// badged and locked like an immutable one, one marked advanced is shown in
// Everything only, and one marked invisible is not shown at all. Essentials
// lists every spec field the CRD has, not only the curated headlines. The CRD and the object are stubbed through the console's own
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
// next to a CRD whose annotations mark spec.vcpuCount operator-managed,
// spec.volumeAutoPlacement.evaluationInterval and spec.tuning advanced, and
// spec.maxSubsystemCount and spec.secrets invisible. The CRD has
// volumeAutoPlacement.disableMigration, which no headline names, and lacks
// volumeAutoPlacement.metricsBackend, which one does.
async function openFixture() {
  const page = await console_.open();
  await page.evaluate(() => {
    const version = (RESOURCES.StorageCluster || {}).version || VERSION;
    const crd = {
      apiVersion: "apiextensions.k8s.io/v1", kind: "CustomResourceDefinition",
      metadata: {name: "storageclusters." + GROUP, annotations: {
        "simplyblock.io/operator-managed-fields": JSON.stringify({[version]: ["spec.vcpuCount"]}),
        "simplyblock.io/invisible-fields": JSON.stringify({[version]: ["spec.maxSubsystemCount", "spec.secrets"]}),
        "simplyblock.io/advanced-fields": JSON.stringify({[version]: ["spec.volumeAutoPlacement.evaluationInterval", "spec.tuning"]}),
      }},
      spec: {group: GROUP, names: {kind: "StorageCluster", plural: "storageclusters"}, versions: [{name: version, served: true, storage: true, schema: {openAPIV3Schema: {
        type: "object", properties: {spec: {type: "object", properties: {
          vcpuCount: {type: "integer", description: "vCPUs per node."},
          maxSubsystemCount: {type: "integer"},
          minHugePagesSize: {type: "string"},
          secrets: {type: "object", properties: {token: {type: "string"}}},
          volumeAutoPlacement: {type: "object", properties: {
            disableMigration: {type: "boolean", description: "Stops the rebalancer creating migrations."},
            evaluationInterval: {type: "string"},
          }},
          tuning: {type: "object", properties: {depth: {type: "integer"}}},
          tolerations: {type: "array", items: {type: "object", properties: {key: {type: "string"}}}},
        }}},
      }}}]},
    };
    const obj = {
      apiVersion: GROUP + "/" + version, kind: "StorageCluster",
      metadata: {name: "fixture", namespace: "sb-fixture", uid: "fixture-uid", generation: 1},
      spec: {vcpuCount: 4, maxSubsystemCount: 128, minHugePagesSize: "2Mi", secrets: {token: "hidden-value"},
        volumeAutoPlacement: {evaluationInterval: "5m"}, tuning: {depth: 3}},
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
  assert.equal(await essentialsRow(page, "secrets.token"), null, "nor is a field below an invisible object");
  await page.context().close();
});

test("Essentials lists a spec field the CRD has and no headline names, unset and editable", async () => {
  const page = await openFixture();
  const row = await essentialsRow(page, "volumeAutoPlacement.disableMigration");
  assert.ok(row, "the volumeAutoPlacement.disableMigration row is shown");
  assert.deepEqual(row.badges, []);
  assert.equal(row.editable, true);
  await page.context().close();
});

// A field inside a list of objects has no single value to show or to patch: its
// path names one field of every element, and an edit would write an object
// over the list.
test("Essentials does not list a field inside a list of objects", async () => {
  const page = await openFixture();
  assert.equal(await essentialsRow(page, "tolerations.key"), null);
  await page.context().close();
});

test("Essentials drops a headline the CRD no longer has", async () => {
  const page = await openFixture();
  assert.equal(await essentialsRow(page, "volumeAutoPlacement.metricsBackend"), null);
  await page.context().close();
});

test("an advanced field is left out of Essentials and kept in Everything", async () => {
  const page = await openFixture();
  assert.equal(await essentialsRow(page, "volumeAutoPlacement.evaluationInterval"), null, "an advanced headline is left out");
  assert.equal(await essentialsRow(page, "tuning.depth"), null, "a field below an advanced object is left out");
  await page.click(".crpanel .tab:has-text('Everything')");
  await page.waitForSelector(".crpanel .jt");
  const keys = await page.$$eval(".crpanel .card .jt .k", els => els.map(e => e.textContent.trim()));
  for (const k of ["evaluationInterval", "tuning", "depth"]) assert.ok(keys.includes(k), `${k} is in the Everything tree`);
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
