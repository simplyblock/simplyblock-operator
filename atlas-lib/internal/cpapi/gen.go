package cpapi

// Regenerate this package from the shared OpenAPI spec (../../../shared/openapi.json,
// also consumed by the operator). The generator version is pinned in go.mod via
// tools.go, so this runs that version rather than @latest. Run `go generate ./...`
// after updating the spec. The resulting cpapi.gen.go is committed.
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config oapi-codegen.yaml ../../../shared/openapi.json

// Then give every response type the UnmarshalJSON that validates it as it is
// deserialized (see validation.go, gen/main.go). This reads the client
// generated above, so it has to run after it.
//
//go:generate go run ./gen -in cpapi.gen.go -out validation.gen.go
