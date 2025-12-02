package utils

import (
	"strconv"
	"strings"
)

func IntPtrOrDefault(ptr *int32, defaultVal int32) int {
	if ptr != nil {
		return int(*ptr)
	}
	return int(defaultVal)
}

func IntPtrOrZero(ptr *int32) int {
	if ptr != nil {
		return int(*ptr)
	}
	return 0
}

func BoolPtrOrFalse(ptr *bool) bool {
	if ptr != nil {
		return *ptr
	}
	return false
}

func BoolPtrToString(ptr *bool) string {
	if ptr != nil && *ptr {
		return "true"
	}
	return "false"
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

