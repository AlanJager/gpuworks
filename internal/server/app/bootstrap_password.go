package app

import (
	"log/slog"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// bootstrapAdminPassword resolves the password used when the admin account is
// created, and reports where it came from so startup can log the right thing.
//
// Precedence: Config.AdminPassword (in-process harnesses) >
// GPUWORKS_ADMIN_PASSWORD (a deployment-time choice, e.g. in a container) > the
// credentials file written by `gpuworks install` > "" (let the store generate
// one). Sources are "config", "env", "file" and "generated".
func bootstrapAdminPassword(cfg Config) (string, string) {
	if cfg.AdminPassword != "" {
		return cfg.AdminPassword, "config"
	}
	if pw := strings.TrimSpace(os.Getenv(adminPasswordEnvVar)); pw != "" {
		return pw, "env"
	}
	if creds, err := installcred.ReadInitialCredentials(cfg.DataDir); err == nil && creds.Password != "" {
		return creds.Password, "file"
	}
	return "", "generated"
}

// adminPasswordEnvVar names the initial admin password on first start. It is
// read only when the admin account is created, never afterwards.
const adminPasswordEnvVar = "GPUWORKS_ADMIN_PASSWORD"

// bootstrapAdminAccount creates the initial admin account and a default API
// key when the store has neither yet, so /v1 is usable immediately after
// first start. The password comes from the configuration,
// GPUWORKS_ADMIN_PASSWORD, the credentials file written by `gpuworks install`, or the store's generator;
// either way it is recorded in the credentials file, which is the durable
// place to read it. A generated password is still printed once for
// compatibility.
func bootstrapAdminAccount(cfg Config, st *store.Store, log *slog.Logger) {
	bootstrapPassword, passwordSource := bootstrapAdminPassword(cfg)
	if username, initialPassword, created, err := st.EnsureAdmin(cfg.AdminUser, bootstrapPassword); err == nil && created {
		credsPath := installcred.CredentialsPath(cfg.DataDir)
		if err := installcred.WriteInitialCredentials(cfg.DataDir, username, initialPassword); err != nil {
			log.Warn("failed to record initial credentials", "path", credsPath, "err", err)
		}
		switch passwordSource {
		case "config":
			log.Info("created admin user with the configured password", "user", username)
		case "env":
			log.Info("created admin user with the password from "+adminPasswordEnvVar, "user", username)
		case "file":
			log.Info("created admin user with the password from the install credentials file", "user", username, "path", credsPath)
		default:
			log.Warn("created admin user with a generated password; save it now because it is shown only once", "user", username, "password", initialPassword)
			log.Info("to choose the initial admin password, set "+adminPasswordEnvVar+" or pass --password to gpuworks install before first start", "path", installcred.CredentialsPath(cfg.DataDir))
		}
		log.Info("initial admin credentials are recorded in this file; change the password and delete it after first login",
			"path", credsPath)
	}
	if !st.HasAnyAPIKey() {
		if _, err := st.CreateAPIKey("default", 0); err == nil {
			log.Warn("created default API key; rotate it before production use")
		}
	}
}
