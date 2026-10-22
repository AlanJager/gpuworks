package store

import (
	"bytes"
	"encoding/json"

	"gorm.io/gorm"
)

// The network role that carries a multi-node group's collectives and a PD
// group's KV transfer was called "parameter" and is called "compute". The
// values below are the ones written before the rename; only migration 118
// knows them.
const (
	legacyComputeRole         = "parameter"
	computeRole               = "compute"
	legacyComputeMountPurpose = "parameter_network"
	computeMountPurpose       = "compute_network"
)

// renameComputeNetwork is migration 118. It renames the role in every value
// that persisted it: a network's roles and a node's management roles off
// (JSON string lists), the network an instance launched on in its schedule
// record (planning.ScheduleRecord, network.parameter becomes network.compute)
// and the purpose of the mount that publishes the accelerator cards' port
// configuration in its desired state (proto.InstanceDesiredState).
//
// Every one of these columns is an opaque JSON snapshot (ADR 0032): the
// database neither selects rows by their inner fields nor updates them in
// place. Each row is read whole, decoded and rewritten whole, and only when
// the rename changes it. The schedule record and the desired state are owned
// by types store cannot import (planning) or must not re-encode (a desired
// state written by an older server may hold keys the current type dropped),
// so they are decoded as raw JSON objects: the renamed key or value changes
// and every other value is kept byte for byte. A value that is not JSON of
// the expected shape is left as it is; its readers already treat it as
// absent.
func renameComputeNetwork(db *gorm.DB) error {
	if err := renameJSONColumn(db, "networks", "roles", renameComputeRoleList); err != nil {
		return err
	}
	if err := renameJSONColumn(db, "nodes", "management_roles_off", renameComputeRoleList); err != nil {
		return err
	}
	if err := renameJSONColumn(db, "instances", "schedule_expl", renameComputeScheduleRecord); err != nil {
		return err
	}
	return renameJSONColumn(db, "instances", "spec", renameComputeMountPurpose)
}

// renameJSONColumn rewrites column of every row of table for which rename
// reports a change. A table or column that does not exist yet (a fresh
// database before AutoMigrate, or one from before the column) passes.
func renameJSONColumn(db *gorm.DB, table, column string, rename func([]byte) ([]byte, bool)) error {
	if !db.Migrator().HasTable(table) {
		return nil
	}
	present, err := tableHasColumn(db, table, column)
	if err != nil || !present {
		return err
	}
	type row struct {
		ID    uint
		Value *string
	}
	var rows []row
	if err := db.Table(table).Select("id, " + column + " AS value").Order("id").Scan(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		if r.Value == nil {
			continue
		}
		renamed, changed := rename([]byte(*r.Value))
		if !changed {
			continue
		}
		if err := db.Table(table).Where("id = ?", r.ID).Update(column, string(renamed)).Error; err != nil {
			return err
		}
	}
	return nil
}

// renameComputeRoleList renames the role in a JSON list of netplan roles. A
// list that already names the new role drops the old one rather than naming
// it twice.
func renameComputeRoleList(raw []byte) ([]byte, bool) {
	var roles []string
	if json.Unmarshal(raw, &roles) != nil {
		return nil, false
	}
	out := make([]string, 0, len(roles))
	changed, seen := false, false
	for _, role := range roles {
		if role == legacyComputeRole {
			role, changed = computeRole, true
		}
		if role == computeRole {
			if seen {
				continue
			}
			seen = true
		}
		out = append(out, role)
	}
	if !changed {
		return nil, false
	}
	encoded, err := json.Marshal(out)
	return encoded, err == nil
}

// renameComputeScheduleRecord moves a schedule record's network.parameter to
// network.compute.
func renameComputeScheduleRecord(raw []byte) ([]byte, bool) {
	var record map[string]json.RawMessage
	if json.Unmarshal(raw, &record) != nil || record == nil {
		return nil, false
	}
	var network map[string]json.RawMessage
	if json.Unmarshal(record["network"], &network) != nil || network == nil {
		return nil, false
	}
	legacy, ok := network[legacyComputeRole]
	if !ok {
		return nil, false
	}
	delete(network, legacyComputeRole)
	if _, renamed := network[computeRole]; !renamed {
		network[computeRole] = legacy
	}
	encodedNetwork, err := json.Marshal(network)
	if err != nil {
		return nil, false
	}
	record["network"] = encodedNetwork
	encoded, err := json.Marshal(record)
	return encoded, err == nil
}

// renameComputeMountPurpose renames the purpose of a desired state's mounts
// (launch.mounts: the purpose was introduced after the resolved launch moved
// under "launch").
func renameComputeMountPurpose(raw []byte) ([]byte, bool) {
	if !bytes.Contains(raw, []byte(legacyComputeMountPurpose)) {
		return nil, false
	}
	var state map[string]json.RawMessage
	if json.Unmarshal(raw, &state) != nil || state == nil {
		return nil, false
	}
	var launch map[string]json.RawMessage
	if json.Unmarshal(state["launch"], &launch) != nil || launch == nil {
		return nil, false
	}
	var mounts []map[string]json.RawMessage
	if json.Unmarshal(launch["mounts"], &mounts) != nil {
		return nil, false
	}
	legacy, _ := json.Marshal(legacyComputeMountPurpose)
	renamed, _ := json.Marshal(computeMountPurpose)
	changed := false
	for _, mount := range mounts {
		if bytes.Equal(mount["purpose"], legacy) {
			mount["purpose"] = renamed
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	encodedMounts, err := json.Marshal(mounts)
	if err != nil {
		return nil, false
	}
	launch["mounts"] = encodedMounts
	encodedLaunch, err := json.Marshal(launch)
	if err != nil {
		return nil, false
	}
	state["launch"] = encodedLaunch
	encoded, err := json.Marshal(state)
	return encoded, err == nil
}
