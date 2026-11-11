package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/acquisition"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func acquisitionDTO(r acquisition.Result) dto.AcquisitionResult {
	to := func(c acquisition.Candidate) dto.AcquisitionCandidate {
		return dto.AcquisitionCandidate{Key: c.Key, Label: c.Label, DisplayName: c.DisplayName, Reason: c.Reason, Recommended: c.Recommended,
			StorageID: c.StorageID, StorageType: c.StorageType, SizeBytes: c.SizeBytes, CacheBytes: c.CacheBytes,
			OnNode: c.OnNode, Unavailable: c.Unavailable, Default: c.Default}
	}
	out := dto.AcquisitionResult{Image: dto.AcquisitionImage{Image: r.Image.Canonical, Cached: r.Image.Cached}}
	for _, c := range r.Present {
		out.Present = append(out.Present, to(c))
	}
	for _, c := range r.Targets {
		out.Targets = append(out.Targets, to(c))
	}
	for _, c := range r.Providers {
		out.Providers = append(out.Providers, to(c))
	}
	return out
}
