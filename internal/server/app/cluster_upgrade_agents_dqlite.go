//go:build linux && dqlite

package app

// heldAgentConnections is how many agent control connections this member
// holds itself, and whether it can say. Agents stay connected to the member
// that accepted them (member_agent_relay.go), so stopping this member makes
// exactly these agents connect again. A member whose business is closed
// holds none and says so; one whose business is being withdrawn cannot say.
func (a *App) heldAgentConnections() (int, bool) {
	current := a.currentClosedBusiness()
	if current == nil {
		return 0, true
	}
	generation, ok := current.(*closedBusinessGeneration)
	if !ok {
		return 0, false
	}
	return generation.agentConnections()
}

func (g *closedBusinessGeneration) agentConnections() (int, bool) {
	g.phaseMu.Lock()
	defer g.phaseMu.Unlock()
	if g.stopping || !g.started || g.business == nil {
		return 0, false
	}
	runtime, ok := g.business.ha.(*memberRuntime)
	if !ok || runtime.forward == nil || runtime.forward.agents == nil {
		return 0, false
	}
	return runtime.forward.agents.relay.Count(), true
}
