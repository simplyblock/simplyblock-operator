# Openings, per language

The mechanics of the opening comment in each language, with examples that exist
in the repository. Every example starts at line 1 of its file, and there is nothing
above it.

## Go: a file in an existing package

A `//` comment directly above the `package` clause, describing this file's
subject. No blank line between the comment and the clause.

```go
// Whitebox tests for the by-id device scan behind Connect and Disconnect.
//
// The scan runs against a fixture directory of real symlinks pointing at real
// files, so filepath.Glob and filepath.EvalSymlinks do the actual work: what is
// under test is glob-pattern semantics against udev's naming, and a faked
// matcher would only ever test the fake.
package util
```

(`csi-driver/pkg/util/initiator_device_test.go`, trimmed.)

Go attaches a comment above `package` to the *package*, so in a multi-file
package only one file should read as the package overview. The others describe
themselves. Start with the subject (`Whitebox tests for…`, `Release of the NVMe
target paths…`) rather than with `Package util…`.

## Go: the package overview

A `doc.go`, or the package's main file, carries the overview as a proper package
comment. It states what the package is for, and it is the right place for the
"why here" sentence.

```go
// Package class classifies an error into what a caller should do about it: the
// gRPC status code to answer with, and whether retrying can help.
//
// Both consumers face the same question and used to answer it separately — the
// CSI driver mapping control-plane failures to RPC statuses, the operator
// deciding whether to requeue a reconcile. Answering it in one place is the point
// of a classifier: it is how a 503 stays retryable and a 400 stays permanent
// everywhere, instead of one component treating a permanent failure as transient
// and retrying it forever.
package class
```

(`atlas-lib/errs/class/class.go`.)

A package that fans out into sub-packages lists them, indented by a tab so
`godoc` renders the block verbatim:

```go
// Public packages, each one cohesive concern:
//
//	nvme         Discover and look up local NVMe controllers/namespaces.
//	nvmeof       Connect/disconnect NVMe-oF (TCP) targets.
```

(`atlas-lib/doc.go`.)

## Go: a command

`// Command <name> …`, the convention for a `main` package:

```go
// Command gen writes validation.gen.go: the UnmarshalJSON methods that make
// every control-plane response type validate itself as it is deserialized, and
// the rule tables they validate against (see ../validation.go for why, and
// ../validation.yaml for what).
package main
```

(`atlas-lib/internal/cpapi/gen/main.go`, trimmed.)

## Python

A module docstring as the very first statement, `"""` on the opening line with a
one-line summary, then a blank line and the detail. A shebang comes above it in
an executable script, and nothing else does.

```python
"""The runner — drives component lifecycles, then runs detectors over the evidence.

Two entry points, and the important thing is that they share the second half:

    Runner.execute()   full run: components through their lifecycle, then judge.
    Runner.judge(ev)   judge alone, over evidence from anywhere — including an archive.

Sharing the judging half is what makes a check fixable against the run that motivated it.
"""
```

(`test/framework/.../sbtest/core/runner.py`.)

## Shell

Shebang, then a `#` block: what the script does, how it is called, and what it
reads from the environment. A script that other things invoke documents its
interface here, because there is no other place to look.

```bash
#!/usr/bin/env bash
#
# Reusable dev-tool downloader for the simplyblock monorepo.
#
# Installs pinned helper tools into <repo-root>/.bin, shared by all components.
#
# Usage:
#   scripts/tools.sh install <tool> [version]   # ensure <tool> in .bin
#   scripts/tools.sh version <tool>             # print the manifest-pinned version
#
# Environment:
#   TOOLS_BIN_DIR       override the install dir (default <repo-root>/.bin)
#   TOOLS_STRICT=1      fail (don't warn) when a tool has no pinned hash
```

(`scripts/tools.sh`, trimmed. `set -euo pipefail` goes below the block.)

## YAML

A `#` header above the document, before any `---`:

```yaml
# Configuration file for MegaLinter
#
# See all available variables at https://megalinter.io/latest/config-file/ and in
# linters documentation
---
APPLY_FIXES: all
```

(`.mega-linter.yml`.)

For a manifest or a test fixture, say what it deploys or reproduces and what it
is for. A bare `apiVersion:` at line 1 leaves the next reader guessing whether
the file is a template, an example, or something that is actually applied.

A Helm template's header is a `#` comment as well. Keep it above the first
`{{-` block. Never hand-write a file into `crds/` or `templates/roles/`, which
`make helm-sync` overwrites.

## Makefile

A `#` block naming what it builds and how it relates to the rest of the build:

```make
# Root Makefile orchestrating build & test across the monorepo components:
#   atlas-lib (shared library), csi-driver, operator
#
# Each target delegates to the relevant component's own Makefile.
```

(`Makefile`.)

## Markdown

The `# H1` is the opening, and a design document or test plan follows the
metadata block defined by the `design-doc` skill. No comment above the H1, and
no license header.

## Go markers are not the opening

`+kubebuilder:` markers, `//go:generate`, and `//nolint` directives sit where
they apply, above the type, the field, or the statement, and not at the top of the
file as a header. A file whose first line is a marker has no opening comment.

The one exception is a package-level marker such as `+kubebuilder:object:generate`
or `+groupName`, which belongs in the package's `doc.go` or `groupversion_info.go`
below the package comment, not above it.
