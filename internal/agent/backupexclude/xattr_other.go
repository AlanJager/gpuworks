//go:build !darwin

package backupexclude

func hasXattr(string) bool { return false }
