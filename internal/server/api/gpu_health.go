package api

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// gpuHealthDetail derives a card's health evidence and vendor commands from
// what the agent stored. The commands are not stored: they follow from the
// vendor, the finding code and the card's PCI address, so a fix to a command
// reaches every card at once. The client renders the explanation for the code
// in its own locale.
func gpuHealthDetail(g *store.GPU) ([]dto.GPUHealthField, *dto.GPUHealthGuide) {
	evidence := accelerator.ParseHealthEvidence(g.HealthMessage)
	var fields []dto.GPUHealthField
	for _, f := range evidence {
		fields = append(fields, dto.GPUHealthField{Name: f.Name, Value: f.Value})
	}
	guide, ok := accelerator.HealthGuideFor(g.Vendor, g.HealthCode, accelerator.HealthTarget(g.PCIeBDF, g.Index), evidence)
	if !ok {
		return fields, nil
	}
	return fields, &dto.GPUHealthGuide{Probe: guide.Probe, Recheck: guide.Recheck, Actions: guide.Actions}
}
