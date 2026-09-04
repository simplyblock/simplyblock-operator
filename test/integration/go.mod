// Integration-test harness: a Talos/QEMU cluster, a fake control plane, and an
// nvmet fabric. Separate module so its Kubernetes and QEMU dependencies stay out
// of atlas-lib and out of the driver it exercises.
module github.com/simplyblock/simplyblock-operator/test/integration

go 1.26.2

require (
	github.com/google/uuid v1.6.0
	github.com/oapi-codegen/oapi-codegen/v2 v2.8.0
	github.com/oapi-codegen/runtime v1.6.0
	github.com/simplyblock/atlas v0.0.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/dprotaso/go-yit v0.0.0-20220510233725-9ba8df137936 // indirect
	github.com/gabriel-vasile/mimetype v1.4.13 // indirect
	github.com/getkin/kin-openapi v0.142.0 // indirect
	github.com/go-openapi/jsonpointer v0.23.1 // indirect
	github.com/go-openapi/swag/jsonname v0.26.0 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.30.3 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/oasdiff/yaml v0.1.1 // indirect
	github.com/oasdiff/yaml3 v0.0.14 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/speakeasy-api/jsonpath v0.6.3 // indirect
	github.com/speakeasy-api/openapi v1.24.0 // indirect
	github.com/vmware-labs/yaml-jsonpath v0.3.2 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// The harness tests atlas's detection against a real kernel, so it must build
// against the tree it sits in, not a published version.
replace github.com/simplyblock/atlas => ../../atlas-lib
