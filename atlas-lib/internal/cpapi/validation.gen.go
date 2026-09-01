// Code generated from cpapi.gen.go and validation.yaml by internal/cpapi/gen. DO NOT EDIT.

// Every response type below decodes through Validate, so a body that does not
// carry what this client was generated to expect fails the decode with
// errs.ErrInvalidResponse instead of yielding plausible zero values. See
// validation.go for the mechanism, validation.yaml for the rules.

package cpapi

import "encoding/json"

// responseRules is validation.yaml, resolved to the generated types.
var responseRules = []responseRule{
	{
		typ: VolumeDTO{},
		rules: map[string]string{
			"Id":       "required",
			"Name":     "required",
			"PoolName": "required",
			"Size":     "gt=0",
			"Nqn":      "required,startswith=nqn.",
		},
		keys: []string{"ns_id"},
	},
	{
		typ: NvmeConnectEntry{},
		rules: map[string]string{
			"Transport": "required",
			"Port":      "required,gt=0,lte=65535",
			"Nqn":       "required,startswith=nqn.",
		},
		keys: []string{"ip"},
	},
	{
		typ: StoragePoolDTO{},
		rules: map[string]string{
			"Id":        "required",
			"ClusterId": "required",
			"Name":      "required",
		},
		keys: []string{"max_size"},
	},
	{
		typ: StorageNodeDTO{},
		rules: map[string]string{
			"Id":        "required",
			"ClusterId": "required",
			"Hostname":  "required",
			"Status":    "required",
			"MgmtIp":    "required,ipv4",
		},
		keys: []string{"lvols", "lvols_max", "device_count", "secondary_node_id"},
	},
	{
		typ: MigrationDTO{},
		rules: map[string]string{
			"Id":           "required",
			"LvolId":       "required",
			"SourceNodeId": "required",
			"TargetNodeId": "required",
			"Phase":        "required",
			"Status":       "required",
		},
		keys: []string{"retry_count", "max_retries", "snaps_migrated", "snaps_total", "error_message"},
	},
	{
		// NvmeConnectEntry's rules, plus what GET /api/v2/clusters/{cluster_id}/storage-pools/{pool_id}/volumes/{volume_id}/connect promises on top.
		typ: LvolConnectEntry{},
		rules: map[string]string{
			"Transport": "required",
			"Port":      "required,gt=0,lte=65535",
			"Nqn":       "required,startswith=nqn.",
			"NsId":      "required,gt=0",
		},
		keys: []string{"ip"},
	},
}

// LvolConnectEntry is a NvmeConnectEntry as answered by GET /api/v2/clusters/{cluster_id}/storage-pools/{pool_id}/volumes/{volume_id}/connect, which promises more
// than the shared model does. Same fields, own identity, so it can carry its
// own rules. Convert to NvmeConnectEntry where the difference does not matter.
type LvolConnectEntry NvmeConnectEntry

// UnmarshalJSON decodes and validates a BackupDTO.
func (d *BackupDTO) UnmarshalJSON(data []byte) error {
	type plain BackupDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = BackupDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a BackupPolicyDTO.
func (d *BackupPolicyDTO) UnmarshalJSON(data []byte) error {
	type plain BackupPolicyDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = BackupPolicyDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a BatchMigrationDTO.
func (d *BatchMigrationDTO) UnmarshalJSON(data []byte) error {
	type plain BatchMigrationDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = BatchMigrationDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a CapacityStatDTO.
func (d *CapacityStatDTO) UnmarshalJSON(data []byte) error {
	type plain CapacityStatDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = CapacityStatDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a ClusterDTO.
func (d *ClusterDTO) UnmarshalJSON(data []byte) error {
	type plain ClusterDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = ClusterDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a DeviceDTO.
func (d *DeviceDTO) UnmarshalJSON(data []byte) error {
	type plain DeviceDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = DeviceDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a DeviceHealthInfoDTO.
func (d *DeviceHealthInfoDTO) UnmarshalJSON(data []byte) error {
	type plain DeviceHealthInfoDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = DeviceHealthInfoDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a FailoverResultDTO.
func (d *FailoverResultDTO) UnmarshalJSON(data []byte) error {
	type plain FailoverResultDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = FailoverResultDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a ManagementNodeDTO.
func (d *ManagementNodeDTO) UnmarshalJSON(data []byte) error {
	type plain ManagementNodeDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = ManagementNodeDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a MigrationDTO.
func (d *MigrationDTO) UnmarshalJSON(data []byte) error {
	type plain MigrationDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = MigrationDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a NvmeConnectEntry.
func (d *NvmeConnectEntry) UnmarshalJSON(data []byte) error {
	type plain NvmeConnectEntry // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = NvmeConnectEntry(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a ReplicationPolicyDTO.
func (d *ReplicationPolicyDTO) UnmarshalJSON(data []byte) error {
	type plain ReplicationPolicyDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = ReplicationPolicyDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a ReplicationRelationshipDTO.
func (d *ReplicationRelationshipDTO) UnmarshalJSON(data []byte) error {
	type plain ReplicationRelationshipDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = ReplicationRelationshipDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a ReplicationTargetDTO.
func (d *ReplicationTargetDTO) UnmarshalJSON(data []byte) error {
	type plain ReplicationTargetDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = ReplicationTargetDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a SnapshotDTO.
func (d *SnapshotDTO) UnmarshalJSON(data []byte) error {
	type plain SnapshotDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = SnapshotDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a StorageNodeDTO.
func (d *StorageNodeDTO) UnmarshalJSON(data []byte) error {
	type plain StorageNodeDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = StorageNodeDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a StoragePoolDTO.
func (d *StoragePoolDTO) UnmarshalJSON(data []byte) error {
	type plain StoragePoolDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = StoragePoolDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a TaskDTO.
func (d *TaskDTO) UnmarshalJSON(data []byte) error {
	type plain TaskDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = TaskDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a VolumeDTO.
func (d *VolumeDTO) UnmarshalJSON(data []byte) error {
	type plain VolumeDTO // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = VolumeDTO(v)
	return Validate(data, d)
}

// UnmarshalJSON decodes and validates a LvolConnectEntry.
func (d *LvolConnectEntry) UnmarshalJSON(data []byte) error {
	type plain LvolConnectEntry // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = LvolConnectEntry(v)
	return Validate(data, d)
}
