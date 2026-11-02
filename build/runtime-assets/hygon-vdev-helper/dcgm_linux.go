//go:build linux && cgo

package main

/*
#cgo CFLAGS: -I/opt/hyhal/include -I/opt/hyhal/include/rocm_smi -I/opt/dtk/include -I/opt/dtk/include/rocm_smi
#include <stdint.h>
#include <stddef.h>
#include <stdbool.h>
#if defined(__has_include)
#if __has_include(<rocm_smi_v2.h>)
#include <rocm_smi_v2.h>
#else
#include <rocm_smi.h>
#endif
#else
#include <rocm_smi.h>
#endif
*/
import "C"

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/HYGON-AI/hcu-dcgm/v3/pkg/dcgm"
)

type nativeAPI struct{}

func newDeviceAPI() deviceAPI { return nativeAPI{} }

func (nativeAPI) Init() error {
	// DCGM Init compares host-wide sysfs and runtime device counts.
	// A notebook may expose one card while retaining the host's full sysfs.
	// Initialize only the runtime needed by the single-device operations below.
	status := C.rsmi_init(0)
	if status == C.RSMI_STATUS_SUCCESS {
		return nil
	}
	var message *C.char
	messageStatus := C.rsmi_status_string(status, &message)
	if messageStatus != C.RSMI_STATUS_SUCCESS || message == nil {
		return fmt.Errorf("rsmi_init(0) failed: status=%d (status text lookup returned %d)", int(status), int(messageStatus))
	}
	return fmt.Errorf("rsmi_init(0) failed: status=%d (%s)", int(status), C.GoString(message))
}

func (nativeAPI) ShutDown() error { return dcgm.ShutDown() }

func (nativeAPI) PhysicalPCIBDF(gpuIndex int) (string, error) {
	return dcgm.PicBusInfo(gpuIndex)
}

func (nativeAPI) Create(gpuIndex, cuCount, memoryMiB int) ([]int, error) {
	// The upstream CreateVDevices API prints its file scan to stdout. Keep
	// stdout reserved for the helper's one-line JSON protocol.
	stdoutFD, err := syscall.Dup(1)
	if err != nil {
		return nil, fmt.Errorf("save stdout: %w", err)
	}
	defer syscall.Close(stdoutFD)
	if err := syscall.Dup2(2, 1); err != nil {
		return nil, fmt.Errorf("redirect DCGM output: %w", err)
	}
	ids, createErr := dcgm.CreateVDevices(gpuIndex, 1, []int{cuCount}, []int{memoryMiB})
	if err := syscall.Dup2(stdoutFD, 1); err != nil {
		return nil, fmt.Errorf("restore stdout after DCGM create: %w", err)
	}
	return ids, createErr
}

func (nativeAPI) Stop(vdevID int) error    { return dcgm.StopVDevice(vdevID) }
func (nativeAPI) Destroy(vdevID int) error { return dcgm.DestroySingleVDevice(vdevID) }

func (nativeAPI) Inspect(vdevID int) (deviceInfo, bool, error) {
	info, err := dcgm.VDeviceSingleInfo(vdevID)
	if err == nil {
		const bytesPerMiB = 1024 * 1024
		if info.VMemoryTotal%bytesPerMiB != 0 || info.VMemoryTotal/bytesPerMiB > uintptr(^uint(0)>>1) {
			return deviceInfo{}, false, fmt.Errorf("vdev %d has invalid memory size %d bytes", vdevID, info.VMemoryTotal)
		}
		pciBDF, err := dcgm.PicBusInfo(info.DvInd)
		if err != nil {
			return deviceInfo{}, false, fmt.Errorf("query vdev %d physical PCI address: %w", vdevID, err)
		}
		if pciBDF == "" {
			return deviceInfo{}, false, fmt.Errorf("vdev %d has empty physical PCI address", vdevID)
		}
		return deviceInfo{GPUIndex: info.DvInd, CUCount: info.VComputeUnitCount, MemoryMiB: int(info.VMemoryTotal / bytesPerMiB), PCIBDF: pciBDF}, true, nil
	}
	// DCGM returns an error string instead of a typed status. Only the
	// explicit absent-device status may be treated as a missing vdev; driver
	// or query failures must remain errors so recovery cannot destroy blindly.
	if isAbsentVDeviceError(err) {
		return deviceInfo{}, false, nil
	}
	return deviceInfo{}, false, err
}

func isAbsentVDeviceError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "vdev not exist") ||
		strings.Contains(message, "vdev_not_exist") ||
		strings.Contains(message, "virtual device does not exist") ||
		strings.Contains(message, "virtual device is not exist")
}
