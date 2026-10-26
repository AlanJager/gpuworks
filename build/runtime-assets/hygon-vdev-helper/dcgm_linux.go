//go:build linux && cgo

package main

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/HYGON-AI/hcu-dcgm/v3/pkg/dcgm"
)

type nativeAPI struct{}

func newDeviceAPI() deviceAPI { return nativeAPI{} }

func (nativeAPI) Init() error     { return dcgm.Init() }
func (nativeAPI) ShutDown() error { return dcgm.ShutDown() }

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
