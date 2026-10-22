package store

import "gpuworks.ai/gpuworks/internal/accelerator"

// NameCount is how many nodes or cards carry one value.
type NameCount struct {
	Name  string
	Count int
}

// FleetSummary is the size and makeup of the fleet, for the update check's
// report: it reads two grouped queries however many nodes there are.
type FleetSummary struct {
	Nodes       int
	OnlineNodes int
	// AgentVersions counts nodes per agent version ("" for a node whose agent
	// has never connected); GPUModels counts the cards each node owns per
	// model, the same cards the scheduler counts as capacity.
	AgentVersions []NameCount
	GPUModels     []NameCount
}

// FleetSummary groups the nodes by agent version and the cards by model.
func (s *Store) FleetSummary() (FleetSummary, error) {
	var out FleetSummary
	var versions []struct {
		Name   string
		Count  int
		Online int
	}
	if err := s.reader().Raw(`SELECT agent_version AS name, COUNT(*) AS count,
		COALESCE(SUM(CASE WHEN state = ? THEN 1 ELSE 0 END), 0) AS online
		FROM nodes GROUP BY agent_version`, NodeStateOnline).Scan(&versions).Error; err != nil {
		return FleetSummary{}, err
	}
	for _, v := range versions {
		out.Nodes += v.Count
		out.OnlineNodes += v.Online
		out.AgentVersions = append(out.AgentVersions, NameCount{Name: v.Name, Count: v.Count})
	}
	var cards []struct {
		Name      string
		Ownership string
		Count     int
	}
	if err := s.reader().Raw(`SELECT name, ownership, COUNT(*) AS count FROM gpus GROUP BY name, ownership`).Scan(&cards).Error; err != nil {
		return FleetSummary{}, err
	}
	byModel := map[string]int{}
	for _, c := range cards {
		if accelerator.CardUsable(c.Ownership) {
			byModel[c.Name] += c.Count
		}
	}
	for name, n := range byModel {
		out.GPUModels = append(out.GPUModels, NameCount{Name: name, Count: n})
	}
	return out, nil
}
