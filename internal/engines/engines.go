// Package engines is the single source of inference engine names and CLI vocabulary.
//
// It is a leaf with only standard-library dependencies. It exists because the names
// are needed on both sides of an unavoidable dependency edge —
// internal/server/deploy owns the engine registry, but internal/agent must
// never import internal/server, and server-side leaf packages reached through
// inferenceapi cannot import deploy without a cycle. Everyone imports this
// package instead; internal/server/deploy re-exports the constants so server
// code can keep naming deploy.EngineVLLM.
package engines

const (
	// VLLM is the vLLM inference server.
	VLLM = "vllm"
	// SGLang is the SGLang inference server.
	SGLang = "sglang"
	// Callable hosts a catalog-owned Serve Adapter rather than a stock engine
	// binary; it has no log dialect and no parameter dictionary of its own.
	Callable = "callable"

	// VLLMOmni marks image catalog entries built with vllm-omni. It is an
	// image family, not a deploy engine: those images run under VLLM, and a
	// vllm deployment may pick them.
	VLLMOmni = "vllm-omni"

	// Default is the engine used when a deployment names none.
	Default = VLLM
)

// CatalogEngines returns the engine values an image catalog entry may declare,
// in the order the image form offers them: every engine in Names plus the
// VLLMOmni image family.
func CatalogEngines() []string { return []string{VLLM, VLLMOmni, SGLang, Callable} }

// DisplayName is an engine's product name ("vLLM", "SGLang"); an unknown name
// is returned as is.
func DisplayName(name string) string {
	switch name {
	case VLLM:
		return "vLLM"
	case VLLMOmni:
		return "vLLM Omni"
	case SGLang:
		return "SGLang"
	case Callable:
		return "Callable"
	default:
		return name
	}
}

// Names returns every registered engine name in sorted order, Callable
// included. Callers that mean "engines with a log dialect and parameter
// dictionary" want DialectNames instead.
func Names() []string { return []string{Callable, SGLang, VLLM} }

// DialectNames returns the engines that produce parseable engine logs and
// carry a parameter dictionary — Names minus Callable.
func DialectNames() []string { return []string{SGLang, VLLM} }
