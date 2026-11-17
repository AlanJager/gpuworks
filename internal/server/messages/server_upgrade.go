package messages

// Server upgrade product errors: what a console upgrade refuses beyond the
// archive itself (api/errors.go holds the rest of the SERVER_UPGRADE_* family).
const (
	// ErrorServerEditionMismatch: the archive holds {release} of the
	// {release_edition} edition, and this server runs the {edition} edition.
	// The console never switches editions; `gpuworks upgrade --archive
	// <file> --allow-edition-change` on the server host does.
	ErrorServerEditionMismatch = "SERVER_EDITION_MISMATCH"
)
