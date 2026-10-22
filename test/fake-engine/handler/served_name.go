package handler

import "strings"

// ServedModel is the name an engine launched with argv answers under, the way
// vLLM and SGLang pick it: --served-model-name when the launch sets one,
// otherwise fallback (the model path for a real engine).
func ServedModel(argv []string, fallback string) string {
	for i, a := range argv {
		if a == "--served-model-name" && i+1 < len(argv) {
			return argv[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--served-model-name="); ok {
			return v
		}
	}
	return fallback
}
