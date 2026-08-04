package controlplane

import (
	"encoding/json"
	"fmt"
)

// The two helpers below only handle the response plumbing every call site
// repeats — checking the status and decoding. Validating what was decoded is
// not their job and no call site's either: the response types validate
// themselves in UnmarshalJSON (see internal/cpapi/validation.go), so a body
// that does not carry what the client needs fails the decode with
// errs.ErrInvalidResponse.

// payload returns the success body of a typed (spec-modelled) response. A nil
// body means the control plane answered with something other than the success
// it declares — respError maps that, including 404 to errs.ErrNotFound. what
// names the request, e.g. `volume <handle>`.
func payload[T any](what string, body *T, code int, raw []byte) (*T, error) {
	if body == nil {
		return nil, respError(what, code, raw)
	}
	return body, nil
}

// decodeBody decodes a response body the spec leaves untyped — several
// endpoints declare no FastAPI response model, so oapi-codegen models them as
// interface{} and the shape has to be spelled out at the call site.
func decodeBody[T any](what string, code int, raw []byte) (T, error) {
	var body T
	if code < 200 || code >= 300 {
		return body, respError(what, code, raw)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return body, fmt.Errorf("%s: decode response: %w", what, err)
	}
	return body, nil
}
