// Precompiles the JSX sources into dist/app.js and dist/mock.js.
// Each file is wrapped in its own function scope, exactly as
// <script type="text/babel"> did — sibling files share only what they publish
// on window. Run from the repository root with @babel/core + preset-react
// installed (the Dockerfile does this); the same script also serves a local
// rebuild after editing any .jsx:  node deploy/build/bundle.mjs
import {readFileSync, writeFileSync, mkdirSync} from "node:fs";
import {createRequire} from "node:module";
const require = createRequire(import.meta.url);
const babel = require("@babel/core");
const preset = require("@babel/preset-react");

const html = readFileSync("index.html", "utf8");
// File order comes from the page itself when it still lists the sources, else
// from build.md's canonical list.
const MOCK = ["mock-backend.jsx", "mock-api.jsx", "k8s-client.jsx", "mock-k8s.jsx", "mock-k8s-server.jsx", "mock-extras.jsx", "mock-repl.jsx", "mock-dr.jsx", "mock-deploy.jsx", "mock-migrate.jsx", "mock-rbac.jsx"];
const APP = ["api.jsx", "agent.jsx", "ui.jsx", "rbac.jsx", "actions.jsx", "panels.jsx", "rbac-admin.jsx", "tiles.jsx", "tiles-data.jsx", "details.jsx", "dr.jsx", "dr-plan.jsx", "repl.jsx", "cgroups.jsx", "migrations.jsx", "k8s.jsx", "deploy.jsx", "deploy-doc.jsx", "storage-types.jsx", "recipe.jsx", "migrate.jsx", "appdr.jsx", "details-data.jsx", "app.jsx"];

const compile = files => files.map(f => {
  const {code} = babel.transformSync(readFileSync(f, "utf8"), {presets: [preset], sourceType: "script", filename: f, babelrc: false, configFile: false, compact: false});
  return `// ---- ${f} ----\n(function(){\n${code}\n})();`;
}).join("\n");

mkdirSync("dist", {recursive: true});
writeFileSync("dist/mock.js", "// generated — do not edit; see build.md\n" + compile(MOCK));
writeFileSync("dist/app.js", "// generated — do not edit; see build.md\n" + compile(APP));
if (!/dist\/app\.js/.test(html)) throw new Error("index.html does not load dist/app.js");
console.log(`bundled ${MOCK.length} mock + ${APP.length} app sources into dist/`);
