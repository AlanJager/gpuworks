package proto

type ImageExistsParams struct {
	ImageRef string `json:"image_ref"`
}

type ImageExistsResult struct {
	Exists bool `json:"exists"`
}
