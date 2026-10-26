//go:build linux && cgo

package main

import (
	"errors"
	"testing"
)

func TestAbsentVDeviceErrorFromZ100Driver(t *testing.T) {
	if !isAbsentVDeviceError(errors.New("The virtual device is not exist.")) {
		t.Fatal("Z100 absent-device status must be classified as missing")
	}
	for _, message := range []string{"Invalid arguments.", "Open mkfd failed.", "Device busy."} {
		if isAbsentVDeviceError(errors.New(message)) {
			t.Fatalf("driver error %q must remain an error", message)
		}
	}
}
