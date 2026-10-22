package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestServerCLIHelp(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_SERVER_CLI") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"gpuworks"}, os.Args[i+1:]...)
				break
			}
		}
		main()
		return
	}
	for _, sub := range []string{"serve", "install", "upgrade", "restart", "stop", "reset-password", "migrate", "expose", "expose up", "expose down", "expose rm", "expose logs", "expose status"} {
		t.Run(sub, func(t *testing.T) {
			args := append([]string{"-test.run=^TestServerCLIHelp$", "--"}, strings.Fields(sub)...)
			cmd := exec.Command(os.Args[0], append(args, "--help")...)
			cmd.Env = append(os.Environ(), "GPUWORKS_TEST_SERVER_CLI=1")
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(strings.ToLower(string(out)), "usage") || strings.Contains(string(out), "flag: help requested") {
				t.Fatalf("help must succeed and print usage without an error: %v\n%s", err, out)
			}
		})
	}
}
