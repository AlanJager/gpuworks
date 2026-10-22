//go:build !linux && !darwin

package storagemount

func defaultStatfs(string) (Usage, error) { return Usage{}, ErrUnsupported }
