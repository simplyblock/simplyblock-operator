package kube

import (
	"fmt"
	"strconv"
)

// StorageClass parameters, VolumeContext, and PublishContext are all
// map[string]string; these helpers read a typed value with a default so the
// operator and CSI driver parse them the same way. A missing or empty value
// yields the default; a present-but-unparsable value yields the default and an
// error (a misconfigured parameter should surface, not be silently ignored).

// StringParam returns params[key], or def when the key is absent or empty.
func StringParam(params map[string]string, key, def string) string {
	if v, ok := params[key]; ok && v != "" {
		return v
	}
	return def
}

// IntParam parses params[key] as a base-10 integer.
func IntParam(params map[string]string, key string, def int) (int, error) {
	v, ok := params[key]
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("parameter %q: %w", key, err)
	}
	return n, nil
}

// FloatParam parses params[key] as a float64.
func FloatParam(params map[string]string, key string, def float64) (float64, error) {
	v, ok := params[key]
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def, fmt.Errorf("parameter %q: %w", key, err)
	}
	return f, nil
}

// BoolParam parses params[key] as a boolean (per strconv.ParseBool:
// 1/t/T/TRUE/true/True and 0/f/F/FALSE/false/False).
func BoolParam(params map[string]string, key string, def bool) (bool, error) {
	v, ok := params[key]
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("parameter %q: %w", key, err)
	}
	return b, nil
}
