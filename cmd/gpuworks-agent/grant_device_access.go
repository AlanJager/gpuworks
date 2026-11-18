package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	udockerruntime "gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
)

func grantDeviceAccess(args []string) error {
	fs := flag.NewFlagSet("grant-device-access", flag.ContinueOnError)
	username := fs.String("user", "", "non-root runtime user")
	stateDir := fs.String("state", defaultStateDir(), "agent state directory")
	createUser := fs.Bool("create-user", false, "create the runtime user when it does not exist (requires root)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*username) == "" {
		return fmt.Errorf("--user is required")
	}
	if *createUser {
		if os.Geteuid() != 0 {
			return fmt.Errorf("root is required to create runtime user %s", *username)
		}
		if _, err := user.Lookup(*username); err != nil {
			var unknown user.UnknownUserError
			if !errors.As(err, &unknown) {
				return fmt.Errorf("look up runtime user %s: %w", *username, err)
			}
			if out, addErr := exec.Command("useradd", "-m", *username).CombinedOutput(); addErr != nil {
				return fmt.Errorf("create runtime user %s: %w: %s", *username, addErr, strings.TrimSpace(string(out)))
			}
		}
	}
	driver := udockerruntime.New(*stateDir)
	detail, err := agentapp.GrantDeviceAccess(context.Background(), gpu.New(), driver, driver.Name(), *username)
	if err != nil {
		return err
	}
	fmt.Println(detail)
	return nil
}
