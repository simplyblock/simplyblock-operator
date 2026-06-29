package sbnqn

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

var (
	testClusterID = uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	testLvolID    = uuid.MustParse("11111111-2222-3333-4444-555555555555")
	testDeviceID  = uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa")
	testNodeUID   = uuid.MustParse("abcdefab-cdef-abcd-efab-cdefabcdefab")
)

func TestParseRoundTrips(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType string
	}{
		{
			name:     "ClusterNQN",
			input:    "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantType: "ClusterNQN",
		},
		{
			name:     "VolumeNQN",
			input:    "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:lvol:11111111-2222-3333-4444-555555555555",
			wantType: "VolumeNQN",
		},
		{
			name:     "TransferhubNQN",
			input:    "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:transferhub:my-hub",
			wantType: "TransferhubNQN",
		},
		{
			name:     "DevNQN",
			input:    "nqn.2014-08.io.simplyblock:storage-node-1:dev:66666666-7777-8888-9999-aaaaaaaaaaaa",
			wantType: "DevNQN",
		},
		{
			name:     "HostNQN",
			input:    "nqn.2014-08.io.simplyblock:uuid:abcdefab-cdef-abcd-efab-cdefabcdefab",
			wantType: "HostNQN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tt.input, err)
			}
			got := parsed.String()
			if got != tt.input {
				t.Errorf("round-trip mismatch:\n  got:  %s\n  want: %s", got, tt.input)
			}

			switch tt.wantType {
			case "ClusterNQN":
				if _, ok := parsed.(ClusterNQN); !ok {
					t.Errorf("expected ClusterNQN, got %T", parsed)
				}
			case "VolumeNQN":
				if _, ok := parsed.(VolumeNQN); !ok {
					t.Errorf("expected VolumeNQN, got %T", parsed)
				}
			case "TransferhubNQN":
				if _, ok := parsed.(TransferhubNQN); !ok {
					t.Errorf("expected TransferhubNQN, got %T", parsed)
				}
			case "DevNQN":
				if _, ok := parsed.(DevNQN); !ok {
					t.Errorf("expected DevNQN, got %T", parsed)
				}
			case "HostNQN":
				if _, ok := parsed.(HostNQN); !ok {
					t.Errorf("expected HostNQN, got %T", parsed)
				}
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"no nqn prefix", "foo.2014-08.io.simplyblock:abc"},
		{"no domain suffix", "nqn.2014-08.io.other:abc"},
		{"empty date", "nqn..io.simplyblock:abc"},
		{"empty body", "nqn.2014-08.io.simplyblock:"},
		{"non-UUID cluster", "nqn.2014-08.io.simplyblock:not-a-uuid"},
		{"unknown 2-segment tag", "nqn.2014-08.io.simplyblock:badtag:abcdefab-cdef-abcd-efab-cdefabcdefab"},
		{"non-UUID host", "nqn.2014-08.io.simplyblock:uuid:not-a-uuid"},
		{"unknown 3-segment tag", "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:unknown:something"},
		{"non-UUID lvol", "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:lvol:bad"},
		{"too many segments", "nqn.2014-08.io.simplyblock:a:b:c:d"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.input)
			if err == nil {
				t.Errorf("Parse(%q) expected error, got nil", tt.input)
			}
		})
	}
}

func TestConstructorRoundTrips(t *testing.T) {
	date := "2014-08"

	tests := []struct {
		name string
		nqn  SBNQN
		want string
	}{
		{
			name: "NewClusterNQN",
			nqn:  NewClusterNQN(date, testClusterID),
			want: "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		},
		{
			name: "NewVolumeNQN",
			nqn:  NewVolumeNQN(date, testClusterID, testLvolID),
			want: "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:lvol:11111111-2222-3333-4444-555555555555",
		},
		{
			name: "NewTransferhubNQN",
			nqn:  NewTransferhubNQN(date, testClusterID, "hub-1"),
			want: "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:transferhub:hub-1",
		},
		{
			name: "NewDevNQN",
			nqn:  NewDevNQN(date, "node-1", testDeviceID),
			want: "nqn.2014-08.io.simplyblock:node-1:dev:66666666-7777-8888-9999-aaaaaaaaaaaa",
		},
		{
			name: "NewHostNQN",
			nqn:  NewHostNQN(date, testNodeUID),
			want: "nqn.2014-08.io.simplyblock:uuid:abcdefab-cdef-abcd-efab-cdefabcdefab",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.nqn.String()
			if got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			// Verify parse round-trip
			parsed, err := Parse(got)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", got, err)
			}
			if parsed.String() != tt.want {
				t.Errorf("parse round-trip: got %q, want %q", parsed.String(), tt.want)
			}
		})
	}
}

func TestExtractLvolID(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID string
		wantOK bool
	}{
		{
			name:   "valid volume NQN",
			input:  "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:lvol:11111111-2222-3333-4444-555555555555",
			wantID: "11111111-2222-3333-4444-555555555555",
			wantOK: true,
		},
		{
			name:   "cluster NQN",
			input:  "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantID: "",
			wantOK: false,
		},
		{
			name:   "host NQN",
			input:  "nqn.2014-08.io.simplyblock:uuid:abcdefab-cdef-abcd-efab-cdefabcdefab",
			wantID: "",
			wantOK: false,
		},
		{
			name:   "garbage string",
			input:  "not-an-nqn",
			wantID: "",
			wantOK: false,
		},
		{
			name:   "empty string",
			input:  "",
			wantID: "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := ExtractLvolID(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ExtractLvolID(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if id != tt.wantID {
				t.Errorf("ExtractLvolID(%q) id = %q, want %q", tt.input, id, tt.wantID)
			}
		})
	}
}

func TestParsedFieldValues(t *testing.T) {
	t.Run("VolumeNQN fields", func(t *testing.T) {
		parsed, err := Parse("nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:lvol:11111111-2222-3333-4444-555555555555")
		if err != nil {
			t.Fatal(err)
		}
		v := parsed.(VolumeNQN)
		if v.ClusterID != testClusterID {
			t.Errorf("ClusterID = %s, want %s", v.ClusterID, testClusterID)
		}
		if v.LvolID != testLvolID {
			t.Errorf("LvolID = %s, want %s", v.LvolID, testLvolID)
		}
	})

	t.Run("HostNQN fields", func(t *testing.T) {
		parsed, err := Parse("nqn.2014-08.io.simplyblock:uuid:abcdefab-cdef-abcd-efab-cdefabcdefab")
		if err != nil {
			t.Fatal(err)
		}
		h := parsed.(HostNQN)
		if h.NodeUID != testNodeUID {
			t.Errorf("NodeUID = %s, want %s", h.NodeUID, testNodeUID)
		}
	})

	t.Run("DevNQN fields", func(t *testing.T) {
		parsed, err := Parse("nqn.2014-08.io.simplyblock:my-node:dev:66666666-7777-8888-9999-aaaaaaaaaaaa")
		if err != nil {
			t.Fatal(err)
		}
		d := parsed.(DevNQN)
		if d.Name != "my-node" {
			t.Errorf("Name = %q, want %q", d.Name, "my-node")
		}
		if d.DeviceID != testDeviceID {
			t.Errorf("DeviceID = %s, want %s", d.DeviceID, testDeviceID)
		}
	})
}

func TestIsZero(t *testing.T) {
	t.Run("zero values", func(t *testing.T) {
		if !(ClusterNQN{}).IsZero() {
			t.Error("zero ClusterNQN should be IsZero")
		}
		if !(VolumeNQN{}).IsZero() {
			t.Error("zero VolumeNQN should be IsZero")
		}
		if !(TransferhubNQN{}).IsZero() {
			t.Error("zero TransferhubNQN should be IsZero")
		}
		if !(DevNQN{}).IsZero() {
			t.Error("zero DevNQN should be IsZero")
		}
		if !(HostNQN{}).IsZero() {
			t.Error("zero HostNQN should be IsZero")
		}
	})

	t.Run("non-zero values", func(t *testing.T) {
		if NewClusterNQN("2014-08", testClusterID).IsZero() {
			t.Error("constructed ClusterNQN should not be IsZero")
		}
		if NewVolumeNQN("2014-08", testClusterID, testLvolID).IsZero() {
			t.Error("constructed VolumeNQN should not be IsZero")
		}
	})

	t.Run("zero String returns empty", func(t *testing.T) {
		if s := (ClusterNQN{}).String(); s != "" {
			t.Errorf("zero ClusterNQN.String() = %q, want empty", s)
		}
		if s := (VolumeNQN{}).String(); s != "" {
			t.Errorf("zero VolumeNQN.String() = %q, want empty", s)
		}
	})
}

func TestJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"ClusterNQN", "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
		{"VolumeNQN", "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:lvol:11111111-2222-3333-4444-555555555555"},
		{"TransferhubNQN", "nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:transferhub:hub-1"},
		{"DevNQN", "nqn.2014-08.io.simplyblock:node-1:dev:66666666-7777-8888-9999-aaaaaaaaaaaa"},
		{"HostNQN", "nqn.2014-08.io.simplyblock:uuid:abcdefab-cdef-abcd-efab-cdefabcdefab"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			data, err := json.Marshal(parsed)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			// Should marshal to a JSON string
			want := `"` + tt.input + `"`
			if string(data) != want {
				t.Errorf("Marshal = %s, want %s", data, want)
			}

			// Unmarshal back into the same type
			switch parsed.(type) {
			case ClusterNQN:
				var v ClusterNQN
				if err := json.Unmarshal(data, &v); err != nil {
					t.Fatalf("Unmarshal: %v", err)
				}
				if v.String() != tt.input {
					t.Errorf("round-trip: got %q, want %q", v.String(), tt.input)
				}
			case VolumeNQN:
				var v VolumeNQN
				if err := json.Unmarshal(data, &v); err != nil {
					t.Fatalf("Unmarshal: %v", err)
				}
				if v.String() != tt.input {
					t.Errorf("round-trip: got %q, want %q", v.String(), tt.input)
				}
			case TransferhubNQN:
				var v TransferhubNQN
				if err := json.Unmarshal(data, &v); err != nil {
					t.Fatalf("Unmarshal: %v", err)
				}
				if v.String() != tt.input {
					t.Errorf("round-trip: got %q, want %q", v.String(), tt.input)
				}
			case DevNQN:
				var v DevNQN
				if err := json.Unmarshal(data, &v); err != nil {
					t.Fatalf("Unmarshal: %v", err)
				}
				if v.String() != tt.input {
					t.Errorf("round-trip: got %q, want %q", v.String(), tt.input)
				}
			case HostNQN:
				var v HostNQN
				if err := json.Unmarshal(data, &v); err != nil {
					t.Fatalf("Unmarshal: %v", err)
				}
				if v.String() != tt.input {
					t.Errorf("round-trip: got %q, want %q", v.String(), tt.input)
				}
			}
		})
	}
}

func TestJSONEmptyStringUnmarshal(t *testing.T) {
	empty := []byte(`""`)

	var c ClusterNQN
	if err := json.Unmarshal(empty, &c); err != nil {
		t.Fatalf("ClusterNQN: %v", err)
	}
	if !c.IsZero() {
		t.Error("ClusterNQN should be zero after empty unmarshal")
	}

	var v VolumeNQN
	if err := json.Unmarshal(empty, &v); err != nil {
		t.Fatalf("VolumeNQN: %v", err)
	}
	if !v.IsZero() {
		t.Error("VolumeNQN should be zero after empty unmarshal")
	}

	var th TransferhubNQN
	if err := json.Unmarshal(empty, &th); err != nil {
		t.Fatalf("TransferhubNQN: %v", err)
	}
	if !th.IsZero() {
		t.Error("TransferhubNQN should be zero after empty unmarshal")
	}

	var d DevNQN
	if err := json.Unmarshal(empty, &d); err != nil {
		t.Fatalf("DevNQN: %v", err)
	}
	if !d.IsZero() {
		t.Error("DevNQN should be zero after empty unmarshal")
	}

	var h HostNQN
	if err := json.Unmarshal(empty, &h); err != nil {
		t.Fatalf("HostNQN: %v", err)
	}
	if !h.IsZero() {
		t.Error("HostNQN should be zero after empty unmarshal")
	}
}

func TestJSONZeroValueMarshal(t *testing.T) {
	tests := []struct {
		name string
		v    any
	}{
		{"ClusterNQN", ClusterNQN{}},
		{"VolumeNQN", VolumeNQN{}},
		{"TransferhubNQN", TransferhubNQN{}},
		{"DevNQN", DevNQN{}},
		{"HostNQN", HostNQN{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.v)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(data) != `""` {
				t.Errorf("zero value marshal = %s, want empty string", data)
			}
		})
	}
}

func TestJSONWrongKindUnmarshal(t *testing.T) {
	volumeNQN := `"nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:lvol:11111111-2222-3333-4444-555555555555"`
	clusterNQN := `"nqn.2014-08.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"`

	var c ClusterNQN
	if err := json.Unmarshal([]byte(volumeNQN), &c); err == nil {
		t.Error("expected error unmarshaling VolumeNQN string into ClusterNQN")
	}

	var v VolumeNQN
	if err := json.Unmarshal([]byte(clusterNQN), &v); err == nil {
		t.Error("expected error unmarshaling ClusterNQN string into VolumeNQN")
	}
}
