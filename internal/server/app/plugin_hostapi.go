package app

import (
	"crypto/hkdf"
	"crypto/sha256"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/server/pluginhost"
)

// pluginKeyInfo prefixes the HKDF info of a plugin's derived keys; the
// package id and the label follow, each after a zero byte
// (docs/reference/plugin-protocol.md). It differs from a compiled module's
// "gpuworks extension key", so a plugin and a module never share a key.
const pluginKeyInfo = "gpuworks plugin key"

// pluginKeys derives plugins' keys from the installation's master key. The
// host API has checked the permission and the bounds of the label and the
// length; the plugin never sees the master key.
type pluginKeys struct{ master string }

var _ pluginhost.KeyDeriver = pluginKeys{}

func (k pluginKeys) DeriveKey(owner pluginhost.Owner, label string, length int) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(k.master), nil, pluginKeyInfo+"\x00"+owner.ID+"\x00"+label, length)
}

// auditClockCorrection is pushed state's ClockCorrected effect: one audit
// line per correction of extclock, whoever made it.
func auditClockCorrection(log *slog.Logger) func(from, to time.Time, by string) {
	return func(from, to time.Time, by string) {
		log.Info("extclock corrected", "audit", true, "by", by,
			"from", from.Format(time.RFC3339), "to", to.Format(time.RFC3339))
	}
}
