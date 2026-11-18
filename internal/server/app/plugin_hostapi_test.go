package app

import (
	"bytes"
	"encoding/hex"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/pluginhost"
)

// A plugin's key is HKDF-SHA256 of the master key with the documented info,
// so it is fixed per package and label (the vector was computed outside Go
// from RFC 5869), differs between packages and labels, and is never a
// compiled module's key for the same name and purpose.
func TestPluginKeysAreBoundToThePackageAndTheLabel(t *testing.T) {
	keys := pluginKeys{master: "0123456789abcdef0123456789abcdef"}
	quota := pluginhost.Owner{ID: "acme_quota"}
	got, err := keys.DeriveKey(quota, "usb.challenge", 32)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != "fc08ed8de76c72ca91b7f3541841e78b87776e4c3561ee0dd6fcf4adc2350468" {
		t.Fatalf("key = %x", got)
	}
	again, _ := keys.DeriveKey(quota, "usb.challenge", 32)
	label, _ := keys.DeriveKey(quota, "license.seal", 32)
	other, _ := keys.DeriveKey(pluginhost.Owner{ID: "acme_backup"}, "usb.challenge", 32)
	if !bytes.Equal(got, again) || bytes.Equal(got, label) || bytes.Equal(got, other) {
		t.Fatal("keys are not bound to exactly the package and the label")
	}
	if short, _ := keys.DeriveKey(quota, "usb.challenge", 16); len(short) != 16 {
		t.Fatalf("a 16-byte key has %d bytes", len(short))
	}
	module := &Host{name: "acme_quota", master: keys.master}
	mk, err := module.DeriveKey("usb.challenge")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(mk, got) {
		t.Fatal("a plugin shares a compiled module's key")
	}
	if rotated, _ := (pluginKeys{master: "another master key of 32 bytes!!"}).DeriveKey(quota, "usb.challenge", 32); bytes.Equal(rotated, got) {
		t.Fatal("the key does not depend on the master key")
	}
}
