package utils

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
