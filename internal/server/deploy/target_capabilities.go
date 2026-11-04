package deploy

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	enginenames "gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func ProfileForTarget(engine, vendor string) (Profile, error) {
	p, err := ProfileForEngine(engine)
	if err != nil {
		return Profile{}, err
	}
	if accelerator.RequiresVendorCatalogImage(vendor) {
		p.DefaultImage = ""
	}
	p.Sleep.Supported = p.Sleep.Supported && accelerator.Supports(vendor, accelerator.FeatureEngineSleep)
	supported := accelerator.Supports(vendor, accelerator.FeatureMultiNode)
	if !supported {
		p.MultiNodeSupported = &supported
	}
	if !accelerator.Supports(vendor, accelerator.FeaturePD) {
		p.PDModes = nil
	} else if v := accelerator.NormalizeVendor(vendor); v != "" {
		// A target's profile lists only the PD modes declared for it.
		modes := p.PDModes[:0:0]
		for _, mode := range p.PDModes {
			for _, declared := range mode.Vendors {
				if accelerator.NormalizeVendor(declared) == v {
					modes = append(modes, mode)
					break
				}
			}
		}
		p.PDModes = modes
	}
	return p, nil
}

// ResolveRequestedFeatures runs after all command overrides and engine hooks.
// Only the executable argv contributes command facts. Preparation scripts and
// shell command text have no engine dialect and remain explicitly unknown.
func ResolveRequestedFeatures(engine string, launch ResolvedLaunch, p Params) *proto.RequestedFeatures {
	args := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
	var valueFlags []string
	for _, spec := range engineParams(engine) {
		if spec.Type != "boolean" && spec.Flag != "" {
			valueFlags = append(valueFlags, spec.Flag)
		}
	}
	features, known := enginenames.CommandFeatures(engine, args, valueFlags...)
	f := &proto.RequestedFeatures{CommandKnown: known, EngineSleep: p.Sleepable, MultiNode: p.NNodes > 1 || p.NodeRank != 0 || p.Headless || p.DistGroup != "" || p.DistAttempt != 0 || (p.DistRole != "" && p.DistRole != "leader" && p.DistRole != "standalone"), PD: p.PD != nil, KVDiskTier: p.Quick.KVDiskOffload, PipelineParallel: p.PipelineParallel > 1, DataParallel: p.Quick.DP > 1}
	for _, feature := range features {
		switch feature {
		case enginenames.EngineSleep:
			f.EngineSleep = true
		case enginenames.MultiNode:
			f.MultiNode = true
		case enginenames.PD:
			f.PD = true
		case enginenames.PipelineParallel:
			f.PipelineParallel = true
		case enginenames.DataParallel:
			f.DataParallel = true
		}
	}
	return f
}

// RequestedLaunchFeatures is retained for callers inspecting an already
// resolved launch. No caller reconstructs a command from request fragments.
func RequestedLaunchFeatures(engine string, launch ResolvedLaunch, p Params) []accelerator.Feature {
	f := launch.RequestedFeatures
	if f == nil {
		f = ResolveRequestedFeatures(engine, launch, p)
	}
	var out []accelerator.Feature
	for _, name := range f.Names() {
		out = append(out, accelerator.Feature(name))
	}
	return out
}
func ValidateTargetLaunch(engine string, launch ResolvedLaunch, p Params) error {
	if accelerator.RequiresDeviceSelection(p.Vendor) && p.AssignedGPUs <= 0 {
		return messages.NewProductError(messages.ErrorAcceleratorDeviceSelectionRequired, "the target requires an explicit assigned GPU set", map[string]any{"vendor": accelerator.VendorBrand(p.Vendor)})
	}
	return ValidateTargetFeatures(engine, launch, p)
}
func ValidateTargetFeatures(engine string, launch ResolvedLaunch, p Params) error {
	snapshot := launch.RequestedFeatures
	if snapshot == nil {
		snapshot = ResolveRequestedFeatures(engine, launch, p)
	}

	for _, f := range RequestedLaunchFeatures(engine, launch, p) {
		if !accelerator.Supports(p.Vendor, f) {
			return messages.NewProductError(messages.ErrorAcceleratorFeatureUnsupported, "target does not support requested accelerator feature", map[string]any{"vendor": accelerator.VendorBrand(p.Vendor), "feature": string(f)})
		}
	}
	if !snapshot.CommandKnown && accelerator.RequiresKnownLaunchFeatures(p.Vendor) {
		return messages.NewProductError(messages.ErrorLaunchFeaturesUnknown, "cannot confirm custom launch features; provide a recognizable executable entrypoint", map[string]any{"vendor": accelerator.VendorBrand(p.Vendor)})
	}
	return nil
}
