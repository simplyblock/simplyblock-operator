package ptr

import (
	"math"
	"strings"
	"testing"
)

func assertPanics(t *testing.T, wantMsg string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q, got none", wantMsg)
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected string panic value, got %T: %v", r, r)
		}
		if !strings.Contains(msg, wantMsg) {
			t.Fatalf("panic message %q does not contain %q", msg, wantMsg)
		}
	}()
	fn()
}

func TestTo(t *testing.T) {
	p := To(42)
	if p == nil || *p != 42 {
		t.Fatalf("To int: got %#v want 42", p)
	}

	s := To("hello")
	if s == nil || *s != "hello" {
		t.Fatalf("To string: got %#v", s)
	}
}

func TestFrom(t *testing.T) {
	v := 7
	if got := From(&v, 3); got != 7 {
		t.Fatalf("From pointer: got %d want 7", got)
	}
	if got := From(nil, 3); got != 3 {
		t.Fatalf("From nil default: got %d want 3", got)
	}

	s := "set"
	if got := From(&s, "def"); got != "set" {
		t.Fatalf("From string pointer: got %q want set", got)
	}
	if got := From(nil, "def"); got != "def" {
		t.Fatalf("From string nil: got %q want def", got)
	}

	// String values are whitespace-trimmed.
	spaced := "  padded \t"
	if got := From(&spaced, "def"); got != "padded" {
		t.Fatalf("From string trim: got %q want padded", got)
	}
}

func TestFromOrZero(t *testing.T) {
	v := 9
	if got := FromOrZero(&v); got != 9 {
		t.Fatalf("FromOrZero pointer: got %d want 9", got)
	}
	if got := FromOrZero[int](nil); got != 0 {
		t.Fatalf("FromOrZero nil: got %d want 0", got)
	}

	f := 1.5
	if got := FromOrZero(&f); got != 1.5 {
		t.Fatalf("FromOrZero float pointer: got %v want 1.5", got)
	}
	if got := FromOrZero[float64](nil); got != 0 {
		t.Fatalf("FromOrZero float nil: got %v want 0", got)
	}
}

func TestIntFrom(t *testing.T) {
	var i int32 = 7
	if got := IntFrom(&i, 3); got != 7 {
		t.Fatalf("IntFrom pointer: got %d want 7", got)
	}
	if got := IntFrom[int32](nil, 3); got != 3 {
		t.Fatalf("IntFrom nil default: got %d want 3", got)
	}

	// Negative value passes through unchanged.
	var neg int64 = -5
	if got := IntFrom(&neg, 0); got != -5 {
		t.Fatalf("IntFrom negative: got %d want -5", got)
	}

	// A uint64 above math.MaxInt saturates rather than wrapping.
	big := uint64(math.MaxUint64)
	if got := IntFrom(&big, 0); got != math.MaxInt {
		t.Fatalf("IntFrom overflow: got %d want %d", got, math.MaxInt)
	}
}

func TestInt64From(t *testing.T) {
	var i int32 = 7
	if got := Int64From(&i, 3); got != 7 {
		t.Fatalf("Int64From pointer: got %d want 7", got)
	}
	if got := Int64From[int32](nil, 3); got != 3 {
		t.Fatalf("Int64From nil default: got %d want 3", got)
	}

	var neg int64 = -5
	if got := Int64From(&neg, 0); got != -5 {
		t.Fatalf("Int64From negative: got %d want -5", got)
	}
}

func TestIntFromOrZero(t *testing.T) {
	var i int32 = 7
	if got := IntFromOrZero(&i); got != 7 {
		t.Fatalf("IntFromOrZero pointer: got %d want 7", got)
	}
	if got := IntFromOrZero[int32](nil); got != 0 {
		t.Fatalf("IntFromOrZero nil: got %d want 0", got)
	}

	var neg int64 = -8
	if got := IntFromOrZero(&neg); got != -8 {
		t.Fatalf("IntFromOrZero negative: got %d want -8", got)
	}

	big := uint64(math.MaxUint64)
	if got := IntFromOrZero(&big); got != math.MaxInt {
		t.Fatalf("IntFromOrZero overflow: got %d want %d", got, math.MaxInt)
	}
}

func TestInt64FromOrZero(t *testing.T) {
	var i int32 = 7
	if got := Int64FromOrZero(&i); got != 7 {
		t.Fatalf("Int64FromOrZero pointer: got %d want 7", got)
	}
	if got := Int64FromOrZero[int32](nil); got != 0 {
		t.Fatalf("Int64FromOrZero nil: got %d want 0", got)
	}

	var neg int64 = -8
	if got := Int64FromOrZero(&neg); got != -8 {
		t.Fatalf("Int64FromOrZero negative: got %d want -8", got)
	}
}

func TestBoolFromOrFalse(t *testing.T) {
	if BoolFromOrFalse(nil) {
		t.Fatalf("BoolFromOrFalse nil: got true want false")
	}

	tr := true
	if !BoolFromOrFalse(&tr) {
		t.Fatalf("BoolFromOrFalse true pointer: got false want true")
	}

	fa := false
	if BoolFromOrFalse(&fa) {
		t.Fatalf("BoolFromOrFalse false pointer: got true want false")
	}
}

func TestBoolFromOrTrue(t *testing.T) {
	if !BoolFromOrTrue(nil) {
		t.Fatalf("BoolFromOrTrue nil: got false want true")
	}

	tr := true
	if !BoolFromOrTrue(&tr) {
		t.Fatalf("BoolFromOrTrue true pointer: got false want true")
	}

	fa := false
	if BoolFromOrTrue(&fa) {
		t.Fatalf("BoolFromOrTrue false pointer: got true want false")
	}
}

func TestClampToInt(t *testing.T) {
	// In-range positive and negative pass through unchanged.
	if got := ClampToInt(42, false); got != 42 {
		t.Fatalf("ClampToInt positive: got %d want 42", got)
	}
	if got := ClampToInt(-42, false); got != -42 {
		t.Fatalf("ClampToInt negative: got %d want -42", got)
	}
	if got := ClampToInt(0, false); got != 0 {
		t.Fatalf("ClampToInt zero: got %d want 0", got)
	}

	// A small unsigned type never overflows int.
	if got := ClampToInt(uint8(255), false); got != 255 {
		t.Fatalf("ClampToInt uint8: got %d want 255", got)
	}

	// math.MaxInt itself is not clamped.
	if got := ClampToInt(uint64(math.MaxInt), false); got != math.MaxInt {
		t.Fatalf("ClampToInt at max: got %d want %d", got, math.MaxInt)
	}

	// One above math.MaxInt saturates.
	if got := ClampToInt(uint64(math.MaxInt)+1, false); got != math.MaxInt {
		t.Fatalf("ClampToInt max+1: got %d want %d", got, math.MaxInt)
	}

	// The largest uint64 saturates.
	if got := ClampToInt(uint64(math.MaxUint64), false); got != math.MaxInt {
		t.Fatalf("ClampToInt MaxUint64: got %d want %d", got, math.MaxInt)
	}
}

func TestClampToIntPanic(t *testing.T) {
	t.Run("in range does not panic", func(t *testing.T) {
		if got := ClampToInt(42, true); got != 42 {
			t.Fatalf("ClampToInt in-range: got %d want 42", got)
		}
		if got := ClampToInt(uint64(math.MaxInt), true); got != math.MaxInt {
			t.Fatalf("ClampToInt at max: got %d want %d", got, math.MaxInt)
		}
	})

	t.Run("overflow panics", func(t *testing.T) {
		assertPanics(t, "too large to fit in int", func() {
			ClampToInt(uint64(math.MaxUint64), true)
		})
	})

	// The underflow branch is a last-resort guard that only fires where int is
	// narrower than int64 (32-bit builds); int64 is the widest signed type in
	// the constraint, so on 64-bit no value can drop below math.MinInt and it
	// is unreachable. We keep the check but do not exercise it here.
}

func TestIsEmptyString(t *testing.T) {
	// Plain string values.
	if !IsEmptyString("") {
		t.Error(`IsEmptyString(""): got false want true`)
	}
	if !IsEmptyString("   \t\n") {
		t.Error("IsEmptyString(whitespace): got false want true")
	}
	if IsEmptyString("x") {
		t.Error(`IsEmptyString("x"): got true want false`)
	}
	if IsEmptyString("  padded  ") {
		t.Error("IsEmptyString(padded content): got true want false")
	}

	// *string values, including nil.
	if !IsEmptyString[*string](nil) {
		t.Error("IsEmptyString(nil *string): got false want true")
	}
	empty := ""
	if !IsEmptyString(&empty) {
		t.Error("IsEmptyString(&\"\"): got false want true")
	}
	blank := "  "
	if !IsEmptyString(&blank) {
		t.Error("IsEmptyString(&whitespace): got false want true")
	}
	set := "value"
	if IsEmptyString(&set) {
		t.Error("IsEmptyString(&\"value\"): got true want false")
	}
}

func TestStringOrDefault(t *testing.T) {
	// nil interface, and typed nil pointers, both yield the default.
	if got := StringOrDefault(nil, "def"); got != "def" {
		t.Fatalf("StringOrDefault nil: got %q want def", got)
	}
	if got := StringOrDefault((*int)(nil), "def"); got != "def" {
		t.Fatalf("StringOrDefault nil pointer: got %q want def", got)
	}

	// Plain values (not just pointers) are accepted and formatted.
	if got := StringOrDefault(42, "def"); got != "42" {
		t.Fatalf("StringOrDefault value: got %q want 42", got)
	}
	if got := StringOrDefault("hi", "def"); got != "hi" {
		t.Fatalf("StringOrDefault string value: got %q want hi", got)
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"string", StringOrDefault(To("hi"), "def"), "hi"},
		{"bool true", StringOrDefault(To(true), "def"), "true"},
		{"bool false", StringOrDefault(To(false), "def"), "false"},
		{"int", StringOrDefault(To(42), "def"), "42"},
		{"int negative", StringOrDefault(To(-42), "def"), "-42"},
		{"int8", StringOrDefault(To(int8(-8)), "def"), "-8"},
		{"int16", StringOrDefault(To(int16(-16)), "def"), "-16"},
		{"int32", StringOrDefault(To(int32(-32)), "def"), "-32"},
		{"int64", StringOrDefault(To(int64(math.MaxInt64)), "def"), "9223372036854775807"},
		{"uint", StringOrDefault(To(uint(7)), "def"), "7"},
		{"uint8", StringOrDefault(To(uint8(255)), "def"), "255"},
		{"uint16", StringOrDefault(To(uint16(65535)), "def"), "65535"},
		{"uint32", StringOrDefault(To(uint32(42)), "def"), "42"},
		{"uint64", StringOrDefault(To(uint64(math.MaxUint64)), "def"), "18446744073709551615"},
		{"uintptr", StringOrDefault(To(uintptr(16)), "def"), "16"},
		{"float32", StringOrDefault(To(float32(1.5)), "def"), "1.5"},
		{"float64", StringOrDefault(To(3.25), "def"), "3.25"},
		{"complex64", StringOrDefault(To(complex64(complex(1, 2))), "def"), "(1+2i)"},
		{"complex128", StringOrDefault(To(complex(1, 2)), "def"), "(1+2i)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("StringOrDefault: got %q want %q", tc.got, tc.want)
			}
		})
	}

	// A type without an explicit case falls back to fmt formatting.
	type point struct{ X, Y int }
	if got := StringOrDefault(To(point{1, 2}), "def"); got != "{1 2}" {
		t.Fatalf("StringOrDefault struct fallback: got %q want {1 2}", got)
	}
}
