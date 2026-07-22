package cpapi

// Regenerate this package from the shared OpenAPI spec (../../../shared/openapi.json,
// also consumed by the operator). The generator version is pinned in go.mod via
// tools.go, so this runs that version rather than @latest. Run `go generate ./...`
// after updating the spec; the resulting cpapi.gen.go is committed.
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config oapi-codegen.yaml ../../../shared/openapi.json
