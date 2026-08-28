package cpapi

// Response validation, the mechanism. What is validated lives in
// validation.yaml, and the tables and the UnmarshalJSON methods below are compiled
// from it into validation.gen.go by internal/cpapi/gen. Only cpapi.gen.go comes
// from oapi-codegen, so regenerating the client cannot clobber any of this.
//
// The generated client happily decodes a response body whose keys no longer
// match the spec it was generated from: a renamed key (`ns-id` becoming
// `ns_id`, say) leaves the Go field at its zero value, and callers then act on
// a plausible-looking zero (connect to namespace 0, publish a 0-byte volume)
// instead of noticing the version skew.
//
// So every response type validates itself as it is deserialized: its generated
// UnmarshalJSON decodes and then calls Validate. Nothing has to be remembered
// at a call site, because validation fires wherever a body is decoded, including
// inside the generated ParseXxxResponse functions and for DTOs nested in other
// DTOs, and failures wrap errs.ErrInvalidResponse, surfacing as the error of
// the client call itself.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"

	"github.com/simplyblock/atlas/errs"
)

// responseRule is what one response type must hold up to: value constraints by
// Go field name, and JSON keys that must be present in the body for the fields
// whose zero value is legitimate and so cannot be told apart from an absent or
// renamed key by a value rule.
type responseRule struct {
	typ   any
	rules map[string]string
	keys  []string
}

// Validate checks a value that was just decoded from data against the rules
// registered for its type. Failures wrap errs.ErrInvalidResponse and name the
// JSON keys that did not hold up.
//
// It is exported for the hand-written response types in the controlplane
// package, whose constraints live in `validate` struct tags. The generated
// UnmarshalJSON methods call it for everything modeled in the spec.
func Validate(data []byte, v any) error {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	if err := checkKeys(t, data); err != nil {
		return err
	}
	var failures validator.ValidationErrors
	err := validate.Struct(v)
	if !errors.As(err, &failures) {
		return err // nil, or a validator misuse there is no recovering from
	}
	return fmt.Errorf("%w: %s", errs.ErrInvalidResponse, describe(failures))
}

// checkKeys reports the required keys of t missing from the body it decoded
// from. A null body has no keys to check: a nullable field is allowed to be
// absent as a whole.
func checkKeys(t reflect.Type, data []byte) error {
	keys := requiredKeys[t]
	if len(keys) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil // not an object, and the decode itself will have said so
	}
	missing := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, ok := raw[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s: no %s in the body — the control plane's field names no longer match the API spec this client was generated from",
		errs.ErrInvalidResponse, t.Name(), strings.Join(missing, ", "))
}

// requiredKeys is responseRules' key half, by response type.
var requiredKeys = keysByType()

func keysByType() map[reflect.Type][]string {
	byType := make(map[reflect.Type][]string, len(responseRules))
	for _, r := range responseRules {
		if len(r.keys) > 0 {
			byType[reflect.TypeOf(r.typ)] = r.keys
		}
	}
	return byType
}

// validate is the shared validator. It is configured once here and only read
// afterward, which is what makes concurrent use safe. Registering rules on a
// validator already in use is not.
var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New()
	// Report the JSON key, not the Go field name: a failure caused by a
	// renamed key should name the key that was looked for.
	v.RegisterTagNameFunc(jsonKey)
	for _, r := range responseRules {
		v.RegisterStructValidationMapRules(r.rules, r.typ)
	}
	return v
}

// jsonKey is the JSON key a field decodes from, for error messages.
func jsonKey(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		return f.Name
	}
	return name
}

// describe renders validation failures as `key: rule` pairs, naming the JSON
// keys that were expected rather than the Go fields behind them.
func describe(failures validator.ValidationErrors) string {
	parts := make([]string, 0, len(failures))
	for _, f := range failures {
		if f.Param() == "" {
			parts = append(parts, fmt.Sprintf("%s: %s", f.Namespace(), f.Tag()))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s=%s", f.Namespace(), f.Tag(), f.Param()))
	}
	return strings.Join(parts, "; ")
}
