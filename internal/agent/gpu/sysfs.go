package gpu

import (
	"os"
	"strconv"
	"strings"
)

// readIntFile reads a small integer from a sysfs file; returns -1 on any error.
func readIntFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1
	}
	return v
}
