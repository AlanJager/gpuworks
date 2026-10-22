package udocker

import "testing"

// cliArgsMutate gates the udocker CLI audit trail: every invocation that
// changes containers, images or the installation itself logs at INFO, pure
// reads stay at Debug. The subcommand is the first non-flag argument because
// udo callers pass flags such as --execmode=P1 or --name= before positionals.
func TestCliArgsMutate(t *testing.T) {
	mutating := [][]string{
		{"create", "--name=gpuworks-1", "img"},
		{"setup", "--execmode=P1", "gpuworks-1"},
		{"setup", "--nvidia", "gpuworks-1"},
		{"rm", "gpuworks-1"},
		{"run", "gpuworks-1", "curl", "-s", "localhost"},
		{"install"},
		{"pull", "img"},
		{"load", "-i=/tmp/a.tar", "repo"},
		{"rmi", "repo"},
		{"tag", "src", "dst"},
	}
	for _, args := range mutating {
		if !cliArgsMutate(args) {
			t.Errorf("cliArgsMutate(%q) = false, want true", args)
		}
	}
	reads := [][]string{
		{"inspect", "gpuworks-1"},
		{"--version"},
		{"images"},
		{"images", "--list", "img"},
		{},
	}
	for _, args := range reads {
		if cliArgsMutate(args) {
			t.Errorf("cliArgsMutate(%q) = true, want false", args)
		}
	}
}
