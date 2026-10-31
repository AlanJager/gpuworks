package app

import (
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// applyXidFindings holds each card at the finding its NVIDIA Xid events call
// for (accelerator.XidFinding) until a runtime check has passed on it after
// the latest such event. A card with no runtime check stays held until the
// host reboots, which starts a new kernel log; an operator can still open it.
func (n *NodeRuntime) applyXidFindings(gpus []proto.GPUInfo, passedSince func(ident string, t time.Time) bool) {
	xid := n.xid.Load()
	if xid == nil {
		return
	}
	for i := range gpus {
		card := &gpus[i]
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if ident == "" || card.PCIeBDF == "" || !accelerator.CardUsable(card.Ownership) {
			continue
		}
		code := ""
		var evidence []accelerator.HealthField
		for _, ev := range xid.Events(card.PCIeBDF) {
			finding := accelerator.XidFinding(ev.Xid)
			if finding == "" || (passedSince != nil && passedSince(ident, ev.At)) {
				continue
			}
			if accelerator.MostSevereHealth(code, finding) == finding {
				code = finding
			}
			evidence = append(evidence, xidEvidence(ev))
		}
		if code == "" || accelerator.MostSevereHealth(card.HealthCode, code) != code {
			continue
		}
		gpu.SetCardFinding(card, code, evidence)
	}
}

// xidEvidence names an Xid the way the kernel log does, with when it was.
func xidEvidence(ev gpu.XidEvent) accelerator.HealthField {
	return accelerator.HealthField{
		Name:  fmt.Sprintf("Xid %d", ev.Xid),
		Value: ev.Message + " (" + ev.At.UTC().Format("2006-01-02 15:04:05Z") + ")",
	}
}
