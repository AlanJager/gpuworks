package store

import (
	"fmt"
	"testing"
	"time"
)

func TestResidencyWindowAndRemovedInstancesUseTheSameInstantAcrossTimezones(t *testing.T) {
	s, node, ids := seedResidency(t)
	// UTC-4 crosses the calendar boundary; UTC+05:45 also checks a fractional offset.
	from := time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)
	var boundaryID uint
	for _, entry := range []struct {
		uuid string
		at   time.Time
	}{
		{"before", from.Add(-time.Nanosecond)},
		{"boundary", from},
		{"after", from.Add(time.Hour)},
	} {
		event := InstanceEvent{
			InstanceUUID: entry.uuid, DeploymentID: ids["alpha"],
			Kind: EventKindResidency, Code: EventInstanceRemoved, CreatedAt: entry.at,
			Data: fmt.Sprintf(`{"node_id":%d,"gpus":[0]}`, node.ID),
		}
		if err := s.DB().Create(&event).Error; err != nil {
			t.Fatal(err)
		}
		if entry.uuid == "boundary" {
			boundaryID = event.ID
		}
	}

	for _, zone := range []struct {
		name   string
		offset int
	}{
		{"UTC", 0},
		{"UTC+08", 8 * 60 * 60},
		{"UTC-04", -4 * 60 * 60},
		{"UTC+05:45", (5*60 + 45) * 60},
	} {
		t.Run(zone.name, func(t *testing.T) {
			since := from.In(time.FixedZone(zone.name, zone.offset))
			firstID, found, err := s.ResidencyWindowStart(since)
			if err != nil || !found || firstID != boundaryID {
				t.Fatalf("window start = (%d, %t, %v), want event %d at inclusive boundary", firstID, found, err, boundaryID)
			}

			byNode, _, err := s.ResidencyInputs([]uint{node.ID}, since)
			if err != nil {
				t.Fatal(err)
			}
			byDeployment, _, err := s.ResidencyInputsOf([]uint{ids["alpha"]}, since)
			if err != nil {
				t.Fatal(err)
			}
			for scope, instances := range map[string][]ResidencyInstance{"node": byNode, "deployment": byDeployment} {
				names := map[string]bool{}
				for _, instance := range instances {
					names[instance.UUID] = true
				}
				if len(instances) != 2 || !names["boundary"] || !names["after"] || names["before"] {
					t.Errorf("%s scope contains %v, want only boundary and after", scope, names)
				}
			}
		})
	}
}
