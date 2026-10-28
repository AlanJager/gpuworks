package modelmetadata

// The names ModelScope's command-line tool leaves in a model directory to
// record where the bytes came from. They are source identity, not model
// content: no engine reads them, and a shared library's copy can hold them
// unreadable (a root_squashed NFS export serves a root:root 0600 file as
// permission denied).
//
// This package is the one owner of the set because it is the leaf both readers
// can import. The three readers must not drift:
//   - parseRepository below marks a directory a repository by these names;
//   - internal/agent/modelscan/identity.go recognizes a ModelScope download by
//     them, and reads the revision out of ModelScopeVersionMarker;
//   - internal/agent/app/modelcopy.go skips them when a model is copied from a
//     shared storage, so an unreadable one can never fail the whole model.
const (
	// ModelScopeMarker is the empty file modelscope-cli writes beside a
	// downloaded repository.
	ModelScopeMarker = ".msc"
	// ModelScopeVersionMarker holds "Revision:<rev>,CreatedAt:<unix>".
	ModelScopeVersionMarker = ".mv"
)

// SourceMarkers lists every name above.
var SourceMarkers = []string{ModelScopeMarker, ModelScopeVersionMarker}

// IsSourceMarker reports whether name is one of SourceMarkers, so a caller that
// walks a directory can skip one without knowing the tool's vocabulary.
func IsSourceMarker(name string) bool {
	for _, marker := range SourceMarkers {
		if name == marker {
			return true
		}
	}
	return false
}
