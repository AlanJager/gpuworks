//go:build !linux && !darwin

package filelock

func Acquire(string) (func() error, error) { return nil, ErrUnsupported }
