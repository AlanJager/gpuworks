package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ComputeUnits queries the selected physical card rather than assuming that
// all models have the same CU count. It is an optional partition capability;
// whole-card inventory does not depend on the virtualization CLI.
func (p *HygonProber) ComputeUnits(ctx context.Context, index int) (int, error) {
	if index < 0 {
		return 0, fmt.Errorf("invalid Hygon physical device index %d", index)
	}
	ctx, cancel := context.WithTimeout(ctx, vendorCLITimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	args := []string{"virtual", "-d", strconv.Itoa(index), "--show-device-info"}
	var out string
	var err error
	if p.command != nil {
		out, err = p.command(ctx, args...)
	} else {
		command, lookupErr := p.lookup("hy-smi")
		if lookupErr != nil {
			return 0, fmt.Errorf("locate Hygon physical compute-unit query: %w", lookupErr)
		}
		// Admission needs the current physical card's answer. Monitoring may
		// use a cached response when the vendor CLI is slow; quotas must not.
		out, err = RunVendorCLI(ctx, nil, command, args...)
	}
	if err != nil {
		return 0, fmt.Errorf("query Hygon physical compute units: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return parseHygonComputeUnits(out, index)
}

func parseHygonComputeUnits(out string, index int) (int, error) {
	var headerSeen, physicalSeen, countSeen bool
	var count int
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch {
		case strings.HasPrefix(key, "Device "):
			device, err := strconv.Atoi(strings.TrimPrefix(key, "Device "))
			if err != nil || device != index || headerSeen || value != "" {
				return 0, fmt.Errorf("unexpected Hygon device header %q for physical device %d", line, index)
			}
			headerSeen = true
		case key == "Physical Device":
			device, err := strconv.Atoi(value)
			if err != nil || device != index || physicalSeen || !headerSeen {
				return 0, fmt.Errorf("unexpected Hygon physical device %q for device %d", value, index)
			}
			physicalSeen = true
		case key == "Compute units":
			var err error
			count, err = strconv.Atoi(value)
			if err != nil || count <= 0 || countSeen || !physicalSeen {
				return 0, fmt.Errorf("invalid Hygon compute units %q for device %d", value, index)
			}
			countSeen = true
		}
	}
	if !headerSeen || !physicalSeen || !countSeen {
		return 0, fmt.Errorf("incomplete Hygon physical compute-unit report for device %d", index)
	}
	return count, nil
}

// PartitionProbeLibraries supplies the host ABI dependencies needed when the
// intentionally small probe image loads this node's DTK and HYHAL libraries.
func (p *HygonProber) PartitionProbeLibraries(ctx context.Context) ([]ProbeLibrary, error) {
	return hygonProbeLibraries(ctx, sharedLibraryPath)
}

func hygonProbeLibraries(ctx context.Context, resolve func(context.Context, string) (string, error)) ([]ProbeLibrary, error) {
	names := []string{"libz.so.1", "libtinfo.so.5", "libtinfo.so.6", "libelf.so.1"}
	libraries := make([]ProbeLibrary, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, err := resolve(ctx, name)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			// DTK releases depend on different tinfo ABIs. Mount each installed
			// SONAME as itself; the HIP probe rejects an incomplete closure.
			if (name == "libtinfo.so.5" || name == "libtinfo.so.6") && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		libraries = append(libraries, ProbeLibrary{Source: path, Name: name})
	}
	return libraries, nil
}
