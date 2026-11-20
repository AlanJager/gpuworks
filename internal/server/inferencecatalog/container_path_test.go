package inferencecatalog

import "testing"

// A template's container paths (asset unpack_to, component mount_path) stay
// out of the directory where the platform puts the files it provides, so a
// template cannot shadow the peers file or the pre-start wrapper.
func TestTemplateContainerPathsStayOutOfThePlatformFileDir(t *testing.T) {
	for _, value := range []string{"/gpuworks", "/gpuworks/peers.json", "/gpuworks/pre-start.sh", "/gpuworks/anything", "/", "relative", "/a/../b"} {
		if validContainerPath(value) {
			t.Errorf("container path %q was accepted", value)
		}
	}
	for _, value := range []string{"/opt/gpuworks/runtime", "/gpuworks-data", "/models/draft"} {
		if !validContainerPath(value) {
			t.Errorf("container path %q was refused", value)
		}
	}
}
