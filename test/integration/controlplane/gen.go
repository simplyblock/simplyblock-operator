package cpsim

// Regenerate the models from the shared OpenAPI spec (../../../shared/openapi.json),
// the same spec atlas-lib's client and the operator are generated from. The
// generator version is pinned in go.mod via tools.go. models.gen.go is committed.
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config oapi-codegen.yaml ../../../shared/openapi.json

// Then stub every endpoint the simulator does not implement, so it satisfies the
// whole generated interface. This reads the generated server and handlers.go, so
// it has to run after the step above.
//
//go:generate go run ./gen -interface cpsim.gen.go -impl handlers.go -out unimplemented.gen.go
