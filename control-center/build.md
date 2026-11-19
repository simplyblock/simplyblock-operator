# Building dist/

The console is written as plain-script JSX files (`*.jsx`). The browser used to
transpile them on every load with Babel standalone, which took 30–40 s on a
cold start. `dist/mock.js` and `dist/app.js` are the precompiled forms;
`index.html` loads those.

**After editing any .jsx, rebuild** (needs @babel/core, @babel/cli, @babel/preset-react):

```
node deploy/build/bundle.mjs
```

Equivalent by hand:

```
npx @babel/cli --presets @babel/preset-react --no-babelrc \
  contexts.jsx mock-backend.jsx mock-api.jsx k8s-client.jsx mock-k8s.jsx mock-k8s-server.jsx mock-extras.jsx mock-repl.jsx mock-dr.jsx mock-drhub.jsx mock-deploy.jsx mock-migrate.jsx mock-rbac.jsx \
  --out-file dist/mock.js
npx @babel/cli --presets @babel/preset-react --no-babelrc \
  contexts.jsx k8s-client.jsx cp-client.jsx store.jsx fleet-model.jsx k8s-sources.jsx api.jsx drhub-api.jsx agent.jsx ui.jsx rbac.jsx actions.jsx panels.jsx alerts.jsx logs.jsx tasks.jsx rbac-admin.jsx tiles.jsx tiles-data.jsx dashboard.jsx details.jsx cr-headlines.jsx cr-view.jsx dr.jsx dr-plan.jsx repl.jsx cgroups.jsx migrations.jsx k8s.jsx deploy.jsx deploy-doc.jsx storage-types.jsx recipe.jsx migrate.jsx appdr.jsx drhub.jsx details-data.jsx app.jsx \
  --out-file dist/app.js
```

Each file must stay in its own function scope (Babel's `--out-file` concatenates
top-level; wrap with an IIFE per file, as the generated files do), because
sibling files share only what they publish on `window`.

Order matters and is the order above: `contexts.jsx` before any transport,
the transports (`k8s-client.jsx`, `cp-client.jsx`) before the store, the store
and `fleet-model.jsx` before `k8s-sources.jsx`, and the sources before
`api.jsx`. The alert, log, and task modules follow `panels.jsx`; the dashboard
follows the tiles; the custom resource views follow `details.jsx`. The modules
that load later are reached through `window` at render time, which is why a
build without one of them still runs. `deploy/build/prepare.mjs` drops
`dist/mock.js` for the production image.

Without a local Babel install, put the packages anywhere and point `NODE_PATH`
at that `node_modules` before running `node deploy/build/bundle.mjs`.
