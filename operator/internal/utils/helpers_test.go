package utils

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"
)

func TestStringSliceHelpers(t *testing.T) {
	in := []string{"a", "b", "c"}
	if !ContainsString(in, "b") {
		t.Fatalf("ContainsString should find b")
	}
	if ContainsString(in, "x") {
		t.Fatalf("ContainsString should not find x")
	}

	out := RemoveString(in, "b")
	if len(out) != 2 || out[0] != "a" || out[1] != "c" {
		t.Fatalf("RemoveString unexpected output: %#v", out)
	}

	if got := JoinList([]string{"x", "y"}); got != "x,y" {
		t.Fatalf("JoinList got %q want x,y", got)
	}
	if got := JoinList(nil); got != "" {
		t.Fatalf("JoinList nil got %q want empty", got)
	}
}

func TestParseSizeAndHumanBytes(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		mode   string
		unit   string
		strict bool
		want   *int64
	}{
		{
			name: "raw number",
			in:   "123",
			mode: "si/iec",
			want: ptr.To(int64(123)),
		},
		{
			name: "iec value",
			in:   "1GiB",
			mode: "si/iec",
			want: ptr.To(int64(1024 * 1024 * 1024)),
		},
		{
			name: "assume unit",
			in:   "2",
			mode: "si/iec",
			unit: "MiB",
			want: ptr.To(int64(2 * 1024 * 1024)),
		},
		{
			name: "invalid unit",
			in:   "2XYZ",
			mode: "si/iec",
			want: nil,
		},
		{
			// Above the old int32 range but valid now that ParseSize returns *int64.
			name: "value above int32 range",
			in:   "3GiB",
			mode: "si/iec",
			want: ptr.To(int64(3 * 1024 * 1024 * 1024)),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSize(tc.in, tc.mode, tc.unit, tc.strict)
			switch {
			case tc.want == nil && got == nil:
				return
			case tc.want == nil && got != nil:
				t.Fatalf("ParseSize got %v want nil", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("ParseSize got nil want %v", *tc.want)
			case *tc.want != *got:
				t.Fatalf("ParseSize got %v want %v", *got, *tc.want)
			}
		})
	}

	if got := HumanBytes(0, "iec"); got != "0 B" {
		t.Fatalf("HumanBytes zero got %q", got)
	}
	if got := HumanBytes(1024, "iec"); got != "1.0 KiB" {
		t.Fatalf("HumanBytes iec got %q", got)
	}
	if got := HumanBytes(1000, "si"); got != "1.0 KB" {
		t.Fatalf("HumanBytes si got %q", got)
	}
	got := ParseSize("20G", "si/iec", "", false)
	if got == nil {
		t.Fatalf("ParseSize got nil want 20000000000")
		return
	}
	if *got != 20_000_000_000 {
		t.Fatalf("ParseSize got %v want 20000000000", *got)
	}
}
