//go:build !linux || !cgo

package main

import "errors"

type unavailableAPI struct{}

func newDeviceAPI() deviceAPI { return unavailableAPI{} }

func (unavailableAPI) Init() error {
	return errors.New("Hygon vdev helper requires Linux with CGO enabled")
}
func (unavailableAPI) ShutDown() error                     { return nil }
func (unavailableAPI) PhysicalPCIBDF(int) (string, error)  { return "", errors.New("unavailable") }
func (unavailableAPI) Create(int, int, int) ([]int, error) { return nil, errors.New("unavailable") }
func (unavailableAPI) Stop(int) error                      { return errors.New("unavailable") }
func (unavailableAPI) Destroy(int) error                   { return errors.New("unavailable") }
func (unavailableAPI) Inspect(int) (deviceInfo, bool, error) {
	return deviceInfo{}, false, errors.New("unavailable")
}
