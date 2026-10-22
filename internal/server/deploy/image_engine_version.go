package deploy

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// An engine release in an image tag is written after the engine's name
// (gpuworks-runner:cuda13.0-vllm0.30.0-r2, gpustack runner:cuda12.9-vllm0.25.1)
// or as the leading part of the tag of an image named after the engine
// (vllm/vllm-openai:v0.30.0).
const imageTagEngineVersionFormat = `(?:^|[^a-z])%s-?v?([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:rc[0-9]+)?)(?:[^0-9]|$)`

var (
	imageTagLeadVersion = regexp.MustCompile(`^v?([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:rc[0-9]+)?)(?:[^0-9.]|$)`)
	// imageTagEnginePatterns caches the per-engine pattern; there are a
	// handful of engines and a launch plan asks for one each time.
	imageTagEnginePatterns sync.Map // engine -> *regexp.Regexp
)

func imageTagEnginePattern(engine string) *regexp.Regexp {
	if cached, ok := imageTagEnginePatterns.Load(engine); ok {
		return cached.(*regexp.Regexp)
	}
	pattern := regexp.MustCompile(fmt.Sprintf(imageTagEngineVersionFormat, regexp.QuoteMeta(engine)))
	imageTagEnginePatterns.Store(engine, pattern)
	return pattern
}

// ImageEngineVersion is the engine release an image carries: the one the image
// catalog records for it when there is one, otherwise what the image's tag
// says unambiguously, otherwise empty. A tag that only carries other versions
// (a CUDA or Python release) says nothing about the engine.
func ImageEngineVersion(engine, ref, catalogVersion string) string {
	if version := strings.TrimSpace(catalogVersion); version != "" {
		return version
	}
	engine = normalizeKey(engine)
	ref = strings.ToLower(strings.TrimSpace(ref))
	if engine == "" || ref == "" {
		return ""
	}
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash {
		return ""
	}
	repository, tag := ref[:colon], ref[colon+1:]
	if match := imageTagEnginePattern(engine).FindStringSubmatch(tag); match != nil {
		return match[1]
	}
	if strings.Contains(repository[slash+1:], engine) {
		if match := imageTagLeadVersion.FindStringSubmatch(tag); match != nil {
			return match[1]
		}
	}
	return ""
}
