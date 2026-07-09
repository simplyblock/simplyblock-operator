package utils

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var exponentMultipliers = []string{"", "K", "M", "G", "T", "P", "E", "Z"}
var uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func IntPtrOrDefault(ptr *int32, defaultVal int32) int {
	if ptr != nil {
		return int(*ptr)
	}
	return int(defaultVal)
}

func Int64PtrOrDefault(ptr *int64, defaultVal int64) int64 {
	if ptr != nil {
		return *ptr
	}
	return defaultVal
}

func IntPtrOrZero(ptr *int32) int {
	if ptr != nil {
		return int(*ptr)
	}
	return 0
}

func IntToInt32Ptr(val int) *int32 {
	v := int32(val)
	return &v
}

func BoolPtrOrFalse(ptr *bool) bool {
	if ptr != nil {
		return *ptr
	}
	return false
}

func BoolPtrToString(ptr *bool) string {
	return strconv.FormatBool(ptr != nil && *ptr)
}

func BoolToString(v bool) string {
	return strconv.FormatBool(v)
}

func BoolPtr(v bool) *bool {
	return &v
}

func ContainsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func RemoveString(slice []string, s string) []string {
	newSlice := []string{}
	for _, v := range slice {
		if v != s {
			newSlice = append(newSlice, v)
		}
	}
	return newSlice
}

func JoinList(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return strings.Join(list, ",")
}

func Int32PtrToString(ptr *int32) string {
	if ptr == nil {
		return ""
	}
	return strconv.FormatInt(int64(*ptr), 10)
}

func ToInt32Ptr(v int64) *int32 {
	if v > math.MaxInt32 || v < math.MinInt32 {
		return nil
	}
	val := int32(v)
	return &val
}

func ToInt64Ptr(v int64) *int64 {
	return &v
}

func parseUnit(unit string, mode string, strict bool) (int, int, error) {
	unit = strings.TrimSpace(unit)

	regexes := map[string]string{
		"si/iec": `^((?P<prefix>[kKMGTPEZ])(?P<binary>i)?)?` + ternary(strict, `B$`, `B?$`),
		"jedec":  `^(?P<prefix>[KMGTPEZ])?` + ternary(strict, `B$`, `B?$`),
	}

	regex, ok := regexes[mode]
	if !ok {
		return 0, 0, fmt.Errorf("invalid mode: %s", mode)
	}

	re := regexp.MustCompile(regex)
	m := re.FindStringSubmatch(unit)
	if m == nil {
		return 0, 0, errors.New("invalid unit")
	}

	prefix := ""
	binary := false

	for i, name := range re.SubexpNames() {
		if name == "prefix" && m[i] != "" {
			prefix = m[i]
		}
		if name == "binary" && m[i] != "" {
			binary = true
		}
	}

	if mode == "jedec" {
		binary = true
	}

	prefix = strings.ToUpper(prefix)

	if strict {
		if (binary && prefix == "K") || (!binary && prefix == "K") {
			return 0, 0, errors.New("invalid K prefix in strict mode")
		}
	}

	expIndex := -1
	for i, p := range exponentMultipliers {
		if p == prefix {
			expIndex = i
			break
		}
	}

	if expIndex < 0 {
		return 0, 0, errors.New("invalid prefix")
	}

	base := 10
	exp := expIndex * 3

	if binary {
		base = 2
		exp = expIndex * 10
	}

	return base, exp, nil
}

func ParseSize(input string, mode string, assumeUnit string, strict bool) *int32 {
	size := ParseSizeInt64(input, mode, assumeUnit, strict)
	if size == nil {
		return nil
	}
	return ToInt32Ptr(*size)
}

func ParseSizeInt64(input string, mode string, assumeUnit string, strict bool) *int64 {
	input = strings.TrimSpace(input)

	if n, err := strconv.ParseInt(input, 10, 64); err == nil {
		if assumeUnit == "" {
			return ToInt64Ptr(n)
		}
		base, exp, err := parseUnit(assumeUnit, mode, strict)
		if err != nil {
			return nil
		}
		return ToInt64Ptr(n * int64Pow(base, exp))
	}

	re := regexp.MustCompile(`^(?P<size>[0-9]+)\s*(?P<unit>\w+)?$`)
	m := re.FindStringSubmatch(input)
	if m == nil {
		return nil
	}

	sizeVal, _ := strconv.ParseInt(m[re.SubexpIndex("size")], 10, 64)
	unit := m[re.SubexpIndex("unit")]
	if unit == "" {
		unit = assumeUnit
	}

	base, exp, err := parseUnit(unit, mode, strict)
	if err != nil {
		return nil
	}

	return ToInt64Ptr(sizeVal * int64Pow(base, exp))
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func int64Pow(base, exp int) int64 {
	result := int64(1)
	for i := 0; i < exp; i++ {
		result *= int64(base)
	}
	return result
}

func HumanBytes(size int64, mode string) string {
	if size <= 0 {
		return "0 B"
	}

	var base float64
	switch mode {
	case "si":
		base = 1000
	default: // "iec"
		base = 1024
	}

	exp := int(math.Log(float64(size)) / math.Log(base))
	if exp >= len(exponentMultipliers) {
		exp = len(exponentMultipliers) - 1
	}

	sizeInUnit := float64(size) / math.Pow(base, float64(exp))
	prefix := exponentMultipliers[exp]

	if mode == "iec" && prefix != "" {
		prefix += "i"
	}

	return fmt.Sprintf("%.1f %sB", sizeInUnit, prefix)
}

func IsUUID(s string) bool {
	return uuidRegex.MatchString(s)
}
