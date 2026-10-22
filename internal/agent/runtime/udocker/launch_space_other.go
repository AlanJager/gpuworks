//go:build !linux && !darwin

package udocker

func freeBytes(string) int64 { return -1 }
