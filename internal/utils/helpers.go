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
