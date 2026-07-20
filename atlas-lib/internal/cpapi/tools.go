//go:build tools

// This file pins the oapi-codegen generator as a module dependency so
// `go generate ./...` (see gen.go) runs a reproducible, go.mod-locked version
// rather than whatever `@latest` resolves to. The "tools" build tag keeps it
// out of every normal build — it is never compiled into the library.
package cpapi

import _ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
