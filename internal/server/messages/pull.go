package messages

// Pull product error codes: where a node may pull an image or download model
// weights from, decided in
// package planning and passed on by the api layer.
const (
	// ErrorImageSourceForbidden: the pull source rules that apply to {node}
	// leave nothing to pull the image from ({route} is where they live).
	ErrorImageSourceForbidden = "IMAGE_SOURCE_FORBIDDEN"
	// ErrorWeightsFetchForbidden: the pull source rules that apply to {node}
	// leave nowhere to download {model}'s weights from ({route} is where
	// they live).
	ErrorWeightsFetchForbidden = "WEIGHTS_FETCH_FORBIDDEN"
	// ErrorModelMetadataForbidden: the site rules chosen for the management
	// node forbid every address of {source} ({route}).
	ErrorModelMetadataForbidden = "MODEL_METADATA_FORBIDDEN"
)
