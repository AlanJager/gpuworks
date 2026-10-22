package selfinstall

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func uninstallMountPoints() ([]string, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	var points []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return nil, fmt.Errorf("malformed mountinfo line")
		}
		// mountinfo escapes spaces, tabs, newlines and backslashes in octal.
		encoded := fields[4]
		var decoded strings.Builder
		for n := 0; n < len(encoded); n++ {
			if encoded[n] == '\\' && n+3 < len(encoded) {
				value, err := strconv.ParseUint(encoded[n+1:n+4], 8, 8)
				if err != nil {
					return nil, fmt.Errorf("malformed mount path: %w", err)
				}
				decoded.WriteByte(byte(value))
				n += 3
			} else {
				decoded.WriteByte(encoded[n])
			}
		}
		points = append(points, decoded.String())
	}
	return points, nil
}
