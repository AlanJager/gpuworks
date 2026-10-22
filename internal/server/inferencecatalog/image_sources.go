package inferencecatalog

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
)

// maxImageRefLength bounds one image_sources entry, in characters: a sanity
// limit matching the width of inference_specs.image. The editor schema
// (specSchema) states the same limit from this constant.
const maxImageRefLength = 512

// normalizeImageSources validates and normalizes a spec's image_sources.
//
// The check is syntactic only. The author states where the image can be pulled
// from and the deployer chooses; the platform does not require a source to pin
// the digest of image, to be a tag or a digest, or to be reachable. What it does
// refuse is an entry that cannot be a pull reference at all: empty, containing
// whitespace, a URL (an image archive address is a property of the platform's
// image catalog, never of a template), longer than a stored reference, or
// repeated. References compare by pullref.RefKey: as the registry resolves
// them, host and repository case folded, tag case-sensitive.
//
// An entry equal to image is dropped. image is always a candidate - tried after
// the declared sources - so naming it again adds nothing, and dropping it keeps
// one canonical form for the export round trip.
func normalizeImageSources(modelID, specName, image string, sources []string) ([]string, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for index, source := range sources {
		source = strings.TrimSpace(source)
		where := fmt.Sprintf("model %s spec %s image_sources[%d]", modelID, specName, index)
		switch {
		case source == "":
			return nil, fmt.Errorf("%s is empty", where)
		case strings.IndexFunc(source, unicode.IsSpace) >= 0:
			return nil, fmt.Errorf("%s %q contains whitespace", where, source)
		case strings.Contains(source, "://"):
			return nil, fmt.Errorf("%s %q must be an image reference, not a URL", where, source)
		case utf8.RuneCountInString(source) > maxImageRefLength:
			return nil, fmt.Errorf("%s exceeds %d characters", where, maxImageRefLength)
		}
		if pullref.SameRef(source, image) {
			continue
		}
		key := pullref.RefKey(source)
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("%s %q is listed twice", where, source)
		}
		seen[key] = struct{}{}
		out = append(out, source)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// DecodeImageSources decodes the inference_specs.image_sources column, whose
// owner type is []string. A blank column declares no sources and is not an
// error; neither is a stored "null" or "[]".
func DecodeImageSources(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var sources []string
	if err := json.Unmarshal([]byte(raw), &sources); err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, nil
	}
	return sources, nil
}

// EncodeImageSources renders a spec's image_sources for the column: blank when
// the spec declares none, so a spec without the key stores what it stored before
// the column existed.
func EncodeImageSources(sources []string) (string, error) {
	if len(sources) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(sources)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
