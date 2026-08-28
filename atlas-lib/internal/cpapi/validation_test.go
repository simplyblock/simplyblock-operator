package cpapi

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

// TestRuledTypesValidateOnDecode catches a validation.gen.go that is out of
// date: rules only fire from the generated UnmarshalJSON methods, so a ruled
// type without one validates nothing.
func TestRuledTypesValidateOnDecode(t *testing.T) {
	for _, r := range responseRules {
		typ := reflect.TypeOf(r.typ)
		if _, ok := reflect.New(typ).Interface().(json.Unmarshaler); !ok {
			t.Errorf("%s has rules but does not validate on decode — re-run go generate ./internal/cpapi/...", typ.Name())
		}
	}
}

// TestUnmarshalRejectsRenamedKey is the case this all exists for: a key the
// client depends on arrives under a new name, the field decodes to its zero
// value, and acting on that zero would be the first anyone hears of it.
func TestUnmarshalRejectsRenamedKey(t *testing.T) {
	const renamed = `{"transport":"tcp","ip":"10.0.0.1","port":4420,"subnqn":"nqn.x","ns-id":1}`

	var e NvmeConnectEntry
	err := json.Unmarshal([]byte(renamed), &e)
	if !errors.Is(err, errs.ErrInvalidResponse) {
		t.Fatalf("decoding a body with a renamed nqn = %v, want ErrInvalidResponse", err)
	}
	if !strings.Contains(err.Error(), "nqn") {
		t.Errorf("error %q does not name the key that was expected", err)
	}
}

func TestUnmarshalConnectEntry(t *testing.T) {
	const body = `{"transport":"tcp","ip":"10.0.0.1","port":4420,"nqn":"nqn.x","ns-id":3}`

	var e NvmeConnectEntry
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatal(err)
	}
	if e.NsId == nil || *e.NsId != 3 {
		t.Errorf("ns-id = %v, want 3", e.NsId)
	}
}

// TestUnmarshalRejectsZeroValues covers the fields a rename leaves at a zero
// that is never legitimate.
func TestUnmarshalRejectsZeroValues(t *testing.T) {
	for name, body := range map[string]string{
		"port 0":          `{"transport":"tcp","ip":"10.0.0.1","port":0,"nqn":"nqn.x","ns-id":1}`,
		"empty transport": `{"transport":"","ip":"10.0.0.1","port":4420,"nqn":"nqn.x","ns-id":1}`,
		"not an nqn":      `{"transport":"tcp","ip":"10.0.0.1","port":4420,"nqn":"subsys1","ns-id":1}`,
		"no ip key":       `{"transport":"tcp","port":4420,"nqn":"nqn.x","ns-id":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			var e NvmeConnectEntry
			if err := json.Unmarshal([]byte(body), &e); !errors.Is(err, errs.ErrInvalidResponse) {
				t.Errorf("err = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

// TestUnmarshalRequiresKeys covers the other half of the rules: fields whose
// zero value is legitimate, where only the key's presence says the decode was
// right.
func TestUnmarshalRequiresKeys(t *testing.T) {
	const node = `{"id":"11111111-1111-1111-1111-111111111111",` +
		`"cluster_id":"22222222-2222-2222-2222-222222222222","hostname":"node1",` +
		`"status":"online","mgmt_ip":"10.0.0.5","lvols":0,%s` +
		`"device_count":0,"secondary_node_id":null}`

	var d StorageNodeDTO
	if err := json.Unmarshal([]byte(strings.Replace(node, "%s", "", 1)), &d); !errors.Is(err, errs.ErrInvalidResponse) {
		t.Errorf("a body without lvols_max = %v, want ErrInvalidResponse", err)
	}
	if err := json.Unmarshal([]byte(strings.Replace(node, "%s", `"lvols_max":0,`, 1)), &d); err != nil {
		t.Errorf("a node reporting 0 of everything is legitimate, got %v", err)
	}
}

// TestUnmarshalValidatesNestedDTOs shows validation reaching DTOs that are not
// the body's own type: a migration carries connect entries.
func TestUnmarshalValidatesNestedDTOs(t *testing.T) {
	migration := func(entry string) string {
		return `{"id":"33333333-3333-3333-3333-333333333333","lvol_id":"v","source_node_id":"s",` +
			`"target_node_id":"t","phase":"pre_created","status":"running","error_message":"",` +
			`"retry_count":0,"max_retries":3,"snaps_migrated":0,"snaps_total":0,` +
			`"connect_strings":[` + entry + `]}`
	}

	var d MigrationDTO
	err := json.Unmarshal([]byte(migration(
		`{"transport":"tcp","ip":"10.0.0.1","port":4420,"nqn":"subsys1","ns-id":1}`)), &d)
	if !errors.Is(err, errs.ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse for the nested connect entry", err)
	}
	if !strings.Contains(err.Error(), "nqn") {
		t.Errorf("error %q does not name the nested key", err)
	}

	// A migration's pre-connect strings have no namespace yet, because the target
	// namespace does not exist at that point, so a null ns-id is legitimate
	// here, unlike in the /connect response.
	if err := json.Unmarshal([]byte(migration(
		`{"transport":"tcp","ip":"10.0.0.1","port":4420,"nqn":"nqn.x","ns-id":null}`)), &d); err != nil {
		t.Errorf("a migration pre-connect entry without a namespace is legitimate, got %v", err)
	}
}

// TestValidateHandWrittenType covers the path the controlplane package's own
// response types take: constraints from `validate` struct tags.
func TestValidateHandWrittenType(t *testing.T) {
	type nic struct {
		ID      string `json:"ID" validate:"required"`
		Address string `json:"Address" validate:"omitempty,ip"`
	}

	if err := Validate([]byte(`{"ID":"nic-1"}`), &nic{ID: "nic-1"}); err != nil {
		t.Errorf("a NIC without an address is legitimate, got %v", err)
	}
	err := Validate([]byte(`{"Address":"10.0.0.1"}`), &nic{Address: "10.0.0.1"})
	if !errors.Is(err, errs.ErrInvalidResponse) {
		t.Errorf("err = %v, want ErrInvalidResponse", err)
	}
}

// TestLvolConnectEntryScopesNamespaceToItsEndpoint covers the endpoint-scoped
// variant: /connect always carries the namespace to attach, while the migration
// pre-connect strings that share the model have none yet, so the requirement
// lives on the variant and not on NvmeConnectEntry.
func TestLvolConnectEntryScopesNamespaceToItsEndpoint(t *testing.T) {
	const noNamespace = `{"transport":"tcp","ip":"10.0.0.1","port":4420,"nqn":"nqn.x","ns-id":null}`

	var base NvmeConnectEntry
	if err := json.Unmarshal([]byte(noNamespace), &base); err != nil {
		t.Errorf("a migration pre-connect entry has no namespace yet, got %v", err)
	}
	var entry LvolConnectEntry
	if err := json.Unmarshal([]byte(noNamespace), &entry); !errors.Is(err, errs.ErrInvalidResponse) {
		t.Errorf("err = %v, want ErrInvalidResponse", err)
	}

	// The variant carries the base's rules too, merged at generation time.
	const badNQN = `{"transport":"tcp","ip":"10.0.0.1","port":4420,"nqn":"subsys1","ns-id":1}`
	err := json.Unmarshal([]byte(badNQN), &entry)
	if !errors.Is(err, errs.ErrInvalidResponse) || !strings.Contains(err.Error(), "nqn") {
		t.Errorf("err = %v, want ErrInvalidResponse naming nqn", err)
	}

	const good = `{"transport":"tcp","ip":"10.0.0.1","port":4420,"nqn":"nqn.x","ns-id":2}`
	if err := json.Unmarshal([]byte(good), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.NsId == nil || *entry.NsId != 2 {
		t.Errorf("ns-id = %v, want 2", entry.NsId)
	}
	// Same layout as the model it scopes, so it converts freely.
	if NvmeConnectEntry(entry).Nqn != "nqn.x" {
		t.Error("conversion to NvmeConnectEntry lost the nqn")
	}
}
