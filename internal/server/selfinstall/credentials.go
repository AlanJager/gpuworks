package selfinstall

import (
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// Where a bootstrap admin password came from, reported to the operator so they
// know whether the server will accept the value shown.
const (
	PasswordFromFlag    = "flag"
	PasswordFromEnv     = "env"
	PasswordFromPrompt  = "prompt"
	PasswordGenerated   = "generated"
	PasswordUnchanged   = "unchanged"
	AdminPasswordEnvVar = "GPUWORKS_ADMIN_PASSWORD"
)

// resolvePassword decides the bootstrap admin password for a fresh install.
//
// Precedence: --password, then GPUWORKS_ADMIN_PASSWORD, then an interactive
// prompt, then a generated one. A quiet or non-interactive run never prompts,
// so an empty password there means "generate" rather than "ask".
//
// The password is returned to be written into <data>/initial-credentials; it is
// never written into the unit file, which is world-readable.
func (i *Installer) resolvePassword(req InstallRequest) (string, string, error) {
	if pw := strings.TrimSpace(req.Password); pw != "" {
		return pw, PasswordFromFlag, nil
	}
	interactive := !req.Quiet && i.io.Terminal()
	if pw := strings.TrimSpace(os.Getenv(AdminPasswordEnvVar)); pw != "" {
		if !interactive {
			return pw, PasswordFromEnv, nil
		}
		// The operator exported a password on purpose; show it back so a typo
		// in the environment does not become the password of a live server.
		i.io.Say("%s is set to: %s", AdminPasswordEnvVar, pw)
		use, err := i.io.Confirm("Use this as the admin password?", true)
		if err != nil {
			return "", "", err
		}
		if use {
			return pw, PasswordFromEnv, nil
		}
	}
	if !interactive {
		return generatedPassword(), PasswordGenerated, nil
	}
	pw, err := i.io.PromptPassword("Admin password (default: random): ")
	if err != nil {
		return "", "", err
	}
	if pw == "" {
		return generatedPassword(), PasswordGenerated, nil
	}
	return pw, PasswordFromPrompt, nil
}

// generatedPassword returns a random bootstrap password. It uses the same
// generator the store falls back to, so a password minted here is
// indistinguishable from one the server would have created itself.
func generatedPassword() string {
	for {
		if pw := auth.NewToken(9); pw != "" {
			return pw
		}
		// auth.NewToken only returns "" when crypto/rand fails, which means the
		// platform has no entropy source at all. Retrying costs nothing and
		// keeps install from writing an empty password.
	}
}
