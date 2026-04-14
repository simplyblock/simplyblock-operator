package utils

import (
	"fmt"
	"strings"
	"time"
)

type FlexTime struct {
	time.Time
}

func (ft *FlexTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}

	// RFC3339 / RFC3339Nano: 2026-02-09T12:41:01.732252+00:00
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		ft.Time = t
		return nil
	}

	// Space-separated: 2026-02-09 12:41:01.732252+00:00
	const layout1 = "2006-01-02 15:04:05.999999-07:00"
	if t, err := time.Parse(layout1, s); err == nil {
		ft.Time = t
		return nil
	}

	// Space-separated without fractions: 2026-02-09 12:41:01+00:00
	const layout2 = "2006-01-02 15:04:05-07:00"
	if t, err := time.Parse(layout2, s); err == nil {
		ft.Time = t
		return nil
	}

	return fmt.Errorf("invalid time format: %q", s)
}
