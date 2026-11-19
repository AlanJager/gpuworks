package kube

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// hamiRegistered is one entry of HAMi's node registration annotation.
type hamiRegistered struct {
	ID      string `json:"id"`
	Count   int    `json:"count"`
	Devmem  int    `json:"devmem"`
	Devcore int    `json:"devcore"`
	Type    string `json:"type"`
	Health  bool   `json:"health"`
	Mode    string `json:"mode,omitempty"`
}

// HAMi publishes both JSON (MarshalNodeDevices) and colon-delimited records
// (EncodeNodeDevices). The latter has seven fields, or nine with index/mode:
// https://github.com/Project-HAMi/HAMi/blob/v2.11.0/pkg/device/devices.go
// An unreadable record invalidates the registration; retaining only readable
// cards would turn unknown ownership into an apparently free card.
func parseHAMiRegistration(s string) ([]hamiRegistered, error) {
	s = strings.TrimSpace(s)
	var entries []hamiRegistered
	var err error
	if strings.HasPrefix(s, "[") {
		err = json.Unmarshal([]byte(s), &entries)
	} else {
		entries, err = parseHAMiLegacyRegistration(s)
	}
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no registered cards")
	}
	seen := map[string]bool{}
	for i, e := range entries {
		if strings.TrimSpace(e.ID) == "" || seen[e.ID] {
			return nil, fmt.Errorf("record %d has an empty or duplicate card ID", i+1)
		}
		seen[e.ID] = true
		if e.Count < 0 || e.Devmem < 0 || e.Devcore < 0 || (e.Health && (e.Count == 0 || e.Devmem == 0)) {
			return nil, fmt.Errorf("card %s has invalid share, memory or compute values", e.ID)
		}
	}
	return entries, nil
}

func parseHAMiLegacyRegistration(s string) ([]hamiRegistered, error) {
	if !strings.Contains(s, ":") {
		return nil, fmt.Errorf("node registration is neither JSON nor colon-delimited card records")
	}
	var entries []hamiRegistered
	for i, record := range strings.Split(strings.TrimSuffix(s, ":"), ":") {
		fields := strings.Split(record, ",")
		if len(fields) != 7 && len(fields) != 9 {
			return nil, fmt.Errorf("record %d has %d fields, expected 7 or 9", i+1, len(fields))
		}
		e := hamiRegistered{ID: fields[0], Type: fields[4]}
		for j, target := range []*int{&e.Count, &e.Devmem, &e.Devcore} {
			n, err := strconv.Atoi(fields[j+1])
			if err != nil {
				return nil, fmt.Errorf("record %d field %d: %w", i+1, j+2, err)
			}
			*target = n
		}
		if _, err := strconv.Atoi(fields[5]); err != nil {
			return nil, fmt.Errorf("record %d NUMA: %w", i+1, err)
		}
		health, err := strconv.ParseBool(fields[6])
		if err != nil {
			return nil, fmt.Errorf("record %d health: %w", i+1, err)
		}
		e.Health = health
		if len(fields) == 9 {
			index, err := strconv.Atoi(fields[7])
			if err != nil || index < 0 {
				return nil, fmt.Errorf("record %d has invalid device index %q", i+1, fields[7])
			}
			e.Mode = fields[8]
		}
		entries = append(entries, e)
	}
	return entries, nil
}
