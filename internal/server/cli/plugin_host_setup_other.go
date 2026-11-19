//go:build !unix

package cli

// dataOwner has no owner to read where files have no uid.
func dataOwner(string) string { return "" }
