package main

import (
	"context"
	"io"
	goruntime "runtime"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agentconf"
)

func TestInstallServiceRefusesWhatItCannotRegister(t *testing.T) {
	unconfigured := t.TempDir()
	configured := t.TempDir()
	if err := agentconf.Write(configured, agentconf.Config{Server: "http://gw:8800", Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "--state is required"},
		{[]string{"--state", unconfigured}, "run `gpuworks-agent configure` first"},
		{[]string{"--server", "http://gw:8800", "--state", configured}, "flag provided but not defined"},
	} {
		err := installService(context.Background(), tc.args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
	if goruntime.GOOS != "darwin" {
		err := installService(context.Background(), []string{"--state", configured}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "macOS only") {
			t.Fatalf("install-service on %s: %v", goruntime.GOOS, err)
		}
	}
}
