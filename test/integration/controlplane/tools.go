//go:build tools

// Pins the generator as a module dependency so `go generate ./...` runs a
// go.mod-locked version rather than whatever @latest resolves to. The build tag
// keeps it out of every normal build.
package cpsim

import _ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
