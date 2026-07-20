package cpapi

// Regenerate this package from the vendored OpenAPI spec (openapi.json). The
// generator version is pinned in go.mod via tools.go, so this runs that
// version rather than @latest. Run `go generate ./...` after updating the
// spec. The generated cpapi.gen.go is gitignored; a fresh checkout must run
// this before building.
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config oapi-codegen.yaml openapi.json
