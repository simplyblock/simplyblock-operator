package lvm

import (
	"context"
	"encoding/json"
	"fmt"
)

// pvsReport and lvsReport match pvs/lvs --reportformat json: one "report",
// holding one field-named array per requested object type. LVM reports every
// field as a string regardless of its underlying type.
type pvsReport struct {
	Report []struct {
		PV []struct {
			VGName string `json:"vg_name"`
		} `json:"pv"`
	} `json:"report"`
}

type lvsReport struct {
	Report []struct {
		LV []struct {
			LVName string `json:"lv_name"`
		} `json:"lv"`
	} `json:"report"`
}

// pvsVGName returns devicePath's vg_name via pvs, or "" if pvs reported none.
// JSON rather than a text scrape: a WARNING: notice pvs/lvs write to stderr
// (a duplicate-PV signature on a byte-level clone, say) never reaches stdout,
// so it can never be mistaken for a field value here.
func (m *Manager) pvsVGName(ctx context.Context, devicePath string) (string, error) {
	out, err := m.exec(ctx, []string{devicePath}, "pvs", "--reportformat", "json", "-o", "vg_name", devicePath)
	if err != nil {
		return "", err
	}
	var report pvsReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		return "", fmt.Errorf("parse pvs report for %s: %w", devicePath, err)
	}
	if len(report.Report) == 0 || len(report.Report[0].PV) == 0 {
		return "", nil
	}
	return report.Report[0].PV[0].VGName, nil
}

// lvsLVNames is ListLogicalVolumes' and listLogicalVolumesOn's shared
// implementation: lvs --reportformat json -o lv_name, scoped to devices (nil
// for unscoped), returning every lv_name field for volumeGroup, in order.
func (m *Manager) lvsLVNames(ctx context.Context, devices []string, volumeGroup string) ([]string, error) {
	out, err := m.exec(ctx, devices, "lvs", "--reportformat", "json", "-o", "lv_name", volumeGroup)
	if err != nil {
		return nil, err
	}
	var report lvsReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		return nil, fmt.Errorf("parse lvs report for %s: %w", volumeGroup, err)
	}
	if len(report.Report) == 0 {
		return nil, nil
	}
	names := make([]string, len(report.Report[0].LV))
	for i, lv := range report.Report[0].LV {
		names[i] = lv.LVName
	}
	return names, nil
}
