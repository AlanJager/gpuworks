//go:build !linux && !darwin

package hostident

func read() string { return "" }
