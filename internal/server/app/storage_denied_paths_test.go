package app

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/storagerules"
)

// A configuration that never set the denylist keeps the built-in one; an
// explicit empty list denies none, and a replacement is served as given.
func TestServerSettingsStorageDeniedPaths(t *testing.T) {
	for _, tc := range []struct {
		configured, want []string
	}{
		{nil, storagerules.DefaultDeniedPaths()},
		{[]string{}, []string{}},
		{[]string{"/opt/gpuworks"}, []string{"/opt/gpuworks"}},
	} {
		got := newServerSettings(Config{StorageDeniedPaths: tc.configured}, nil, nil).StorageDeniedPaths()
		if got == nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("configured %v served as %v, want %v", tc.configured, got, tc.want)
		}
	}
	if got := DefaultConfig().StorageDeniedPaths; !reflect.DeepEqual(got, storagerules.DefaultDeniedPaths()) {
		t.Fatalf("default configuration denylist = %v", got)
	}
}
