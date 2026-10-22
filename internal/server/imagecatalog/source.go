package imagecatalog

import (
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
)

// The reference vocabulary lives in the pullref leaf package so the lower
// store layer can share one implementation. These aliases keep the catalog's
// own vocabulary stable for its callers.
const (
	SourceQuay        = pullref.SourceQuay
	SourceHuaweiCloud = pullref.SourceHuaweiCloud
	SourceDefault     = pullref.SourceDefault
)

// PullSource returns a compact source label suitable for image_nodes.source
// and operator-facing audit messages.
func PullSource(ref, canonical string) string { return pullref.Source(ref, canonical) }

// NormalizePullRef expands an image reference using Docker registry rules.
func NormalizePullRef(ref string) string { return pullref.NormalizeRef(ref) }

// SameImageRef reports whether two references name the same image (see
// pullref.SameRef).
func SameImageRef(a, b string) bool { return pullref.SameRef(a, b) }
