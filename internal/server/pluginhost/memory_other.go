//go:build !linux && !darwin

package pluginhost

type noMemory struct{}

func defaultMemoryReader() MemoryReader { return noMemory{} }

func (noMemory) GroupRSS(int) (int64, error) { return 0, errUnsupported }
