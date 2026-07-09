package utils

import "testing"

func TestIntAndBoolHelpers(t *testing.T) {
	var i int32 = 7
	if got := IntPtrOrDefault(&i, 3); got != 7 {
		t.Fatalf("IntPtrOrDefault pointer: got %d want 7", got)
	}
	if got := IntPtrOrDefault(nil, 3); got != 3 {
		t.Fatalf("IntPtrOrDefault default: got %d want 3", got)
	}
	if got := IntPtrOrZero(&i); got != 7 {
		t.Fatalf("IntPtrOrZero pointer: got %d want 7", got)
	}
	if got := IntPtrOrZero(nil); got != 0 {
		t.Fatalf("IntPtrOrZero nil: got %d want 0", got)
	}

	if p := IntToInt32Ptr(9); p == nil || *p != 9 {
		t.Fatalf("IntToInt32Ptr: got %#v", p)
	}

	if BoolPtrOrFalse(nil) {
		t.Fatalf("BoolPtrOrFalse nil should be false")
	}
	bTrue := true
	if !BoolPtrOrFalse(&bTrue) {
		t.Fatalf("BoolPtrOrFalse pointer should be true")
	}
	if BoolPtrToString(nil) != "false" {
		t.Fatalf("BoolPtrToString nil should be false")
	}
	if BoolPtrToString(&bTrue) != "true" {
		t.Fatalf("BoolPtrToString true should be true")
	}
	if BoolToString(false) != "false" {
		t.Fatalf("BoolToString false should be false")
	}
	if BoolToString(true) != "true" {
		t.Fatalf("BoolToString true should be true")
	}
}

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
		want   *int32
	}{
		{
			name: "raw number",
			in:   "123",
			mode: "si/iec",
			want: ToInt32Ptr(123),
		},
		{
			name: "iec value",
			in:   "1GiB",
			mode: "si/iec",
			want: ToInt32Ptr(1024 * 1024 * 1024),
		},
		{
			name: "assume unit",
			in:   "2",
			mode: "si/iec",
			unit: "MiB",
			want: ToInt32Ptr(2 * 1024 * 1024),
		},
		{
			name: "invalid unit",
			in:   "2XYZ",
			mode: "si/iec",
			want: nil,
		},
		{
			name: "overflow returns nil",
			in:   "3GiB",
			mode: "si/iec",
			want: nil,
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
	if got := ParseSizeInt64("20G", "si/iec", "", false); got == nil || *got != 20_000_000_000 {
		if got == nil {
			t.Fatalf("ParseSizeInt64 got nil want 20000000000")
		}
		t.Fatalf("ParseSizeInt64 got %v want 20000000000", *got)
	}
}
