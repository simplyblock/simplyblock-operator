// Package ptr provides helpers for pointers to values, which generated API
// request bodies and Kubernetes types use pervasively for optional fields.
package ptr

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// integer constrains the numeric helpers to Go's built-in integer types.
type integer interface {
	int | int8 | int16 | int32 | int64 | uint8 | uint16 | uint32 | uint64
}

// number constrains helpers to any built-in integer or floating-point type.
type number interface {
	integer | float32 | float64
}

// To returns a pointer to v.
func To[T any](v T) *T {
	return &v
}

// From returns the value ptr points to, or def when ptr is nil. When the
// pointed-to value is a string, surrounding whitespace is trimmed.
func From[T any](ptr *T, def T) T {
	if ptr == nil {
		return def
	}
	val := *ptr
	if s, ok := any(val).(string); ok {
		return any(strings.TrimSpace(s)).(T)
	}
	return val
}

// FromOrZero returns the value ptr points to, or the zero value when ptr is nil.
func FromOrZero[T number](ptr *T) T {
	if ptr == nil {
		return 0
	}
	return *ptr
}

// IntFrom returns the value ptr points to as an int, or def when ptr is nil.
// The value is saturated to the int range via ClampToInt.
func IntFrom[T integer](ptr *T, def int) int {
	return ClampToInt(From(ptr, T(def)), false)
}

// Int64From returns the value ptr points to as an int64, or def when ptr is nil.
func Int64From[T integer](ptr *T, def int64) int64 {
	return int64(From(ptr, T(def)))
}

// IntFromOrZero returns the value ptr points to as an int, or 0 when ptr is nil.
// The value is saturated to the int range via ClampToInt.
func IntFromOrZero[T integer](ptr *T) int {
	return ClampToInt(From(ptr, 0), false)
}

// Int64FromOrZero returns the value ptr points to as an int64, or 0 when ptr is nil.
func Int64FromOrZero[T integer](ptr *T) int64 {
	return int64(From(ptr, 0))
}

// BoolFromOrFalse returns the value ptr points to, or false when ptr is nil.
func BoolFromOrFalse(ptr *bool) bool {
	if ptr == nil {
		return false
	}
	return *ptr
}

// BoolFromOrTrue returns the value ptr points to, or true when ptr is nil.
func BoolFromOrTrue(ptr *bool) bool {
	if ptr == nil {
		return true
	}
	return *ptr
}

// StringOrDefault renders val as a string, returning def when val is nil or a
// nil pointer. It accepts either a value or a pointer to one, so optional
// fields (which are pointers) and plain values can be formatted the same way.
func StringOrDefault(val any, def string) string {
	if val == nil {
		return def
	}
	// Unwrap a pointer of any type; a nil pointer yields the default.
	if rv := reflect.ValueOf(val); rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return def
		}
		val = rv.Elem().Interface()
	}
	switch v := val.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.FormatInt(int64(v), 10)
	case int8:
		return strconv.FormatInt(int64(v), 10)
	case int16:
		return strconv.FormatInt(int64(v), 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint8:
		return strconv.FormatUint(uint64(v), 10)
	case uint16:
		return strconv.FormatUint(uint64(v), 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case uintptr:
		return strconv.FormatUint(uint64(v), 10)
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case complex64:
		return strconv.FormatComplex(complex128(v), 'g', -1, 64)
	case complex128:
		return strconv.FormatComplex(v, 'g', -1, 128)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ClampToInt narrows any integer value to int, saturating at math.MaxInt /
// math.MinInt instead of silently truncating or wrapping. This matters for
// values wider than int (uint64, or the 64-bit types on 32-bit builds).
func ClampToInt[T integer](v T, shouldPanic bool) int {
	if v > 0 && uint64(v) > uint64(math.MaxInt) {
		if shouldPanic {
			panic("integer value is too large to fit in int")
		}
		return math.MaxInt
	}
	if v < 0 && int64(v) < int64(math.MinInt) {
		if shouldPanic {
			panic("integer value is too small to fit in int")
		}
		return math.MinInt
	}
	return int(v)
}
