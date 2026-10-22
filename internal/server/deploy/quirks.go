package deploy

import (
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// quirk is a data-driven correction: when match(p) is true, apply(b,p,env) runs
// after quick-params and before user extra_args. Quirks use injectKV/injectFlag,
// so they never override a flag the user set.
type quirk struct {
	name  string
	match func(p Params) bool
	apply func(b *builder, p Params, env map[string]string)
}

// quirks is the ordered correction table. New model families = new entries
// here, tracking vllm's support cadence (plan §11). Deliberately kept small:
// every entry is a deviation from stock engine behaviour that must be
// justified.
var quirks = []quirk{
	{
		name: "deepseek-mla-fp8-kv",
		// DeepSeek MLA runs its KV cache in fp8 (fp8_ds_mla); force kv-cache-dtype.
		match: func(p Params) bool {
			if vendorIs(p, accelerator.VendorAscend) {
				return false
			}
			return containsFold(p.ModelType, "deepseek") || containsFold(p.Arch, "deepseek")
		},
		apply: func(b *builder, _ Params, _ map[string]string) {
			b.injectKV("--kv-cache-dtype", "fp8")
		},
	},
	{
		name: "minimax-block-size",
		match: func(p Params) bool {
			return containsFold(p.ModelType, "minimax") || containsFold(p.Arch, "minimax")
		},
		apply: func(b *builder, _ Params, _ map[string]string) {
			b.injectKV("--block-size", "128")
		},
	},
	{
		name: "apple-loopback",
		// vLLM's engine core joins its process group over gloo on the loopback
		// interface named here, and macOS calls it lo0: with Linux's "lo" the
		// engine dies at startup (measured on a MacBook Pro with vllm-metal 0.31).
		// A Mac has no NCCL to point anywhere.
		match: func(p Params) bool { return vendorIs(p, accelerator.VendorApple) },
		apply: func(_ *builder, _ Params, env map[string]string) {
			env["GLOO_SOCKET_IFNAME"] = "lo0"
			delete(env, "NCCL_SOCKET_IFNAME")
		},
	},
	{
		name: "turing-no-bf16",
		// NVIDIA cards below compute capability 8.0 lack native bf16 support.
		// The decision comes from the probed hardware fact, never from an image
		// label or a free-form architecture name.
		match: func(p Params) bool {
			if !vendorIs(p, accelerator.VendorNVIDIA) {
				return false
			}
			cc, err := strconv.ParseFloat(strings.TrimSpace(p.ComputeCapability), 64)
			return err == nil && cc < 8.0
		},
		apply: func(b *builder, _ Params, _ map[string]string) {
			b.injectKV("--dtype", "float16")
		},
	},
}

func applyQuirks(b *builder, p Params, env map[string]string) {
	for _, q := range quirks {
		if q.match(p) {
			q.apply(b, p, env)
		}
	}
}

func containsFold(s, sub string) bool {
	return s != "" && strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func vendorIs(p Params, vendor string) bool {
	return strings.EqualFold(strings.TrimSpace(p.Vendor), vendor)
}
