package dto

import "gpuworks.ai/gpuworks/pkg/localization"

type URLRewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type NetworkRewritePreset struct {
	ID       string            `json:"id"`
	Name     localization.Text `json:"name"`
	Rewrites []URLRewrite      `json:"rewrites"`
}

type NetworkDNSPreset struct {
	ID      string            `json:"id"`
	Name    localization.Text `json:"name"`
	Advice  localization.Text `json:"advice"`
	Servers []string          `json:"servers"`
}

type NetworkPresets struct {
	RewritePresets []NetworkRewritePreset `json:"rewrite_presets"`
	DNSPresets     []NetworkDNSPreset     `json:"dns_presets"`
}

type ImageRegistryPreset struct {
	Value  string            `json:"value"`
	Label  localization.Text `json:"label"`
	Mirror string            `json:"mirror"`
}
