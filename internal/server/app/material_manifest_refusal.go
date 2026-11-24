//go:build linux && dqlite

package app

import (
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// materialManifestRefusal is one reason the manifest receiver answers 503. Its
// text is the response body.
type materialManifestRefusal string

const (
	manifestNotCurrent            materialManifestRefusal = "material manifest is not current or complete"
	manifestInterrupted           materialManifestRefusal = "material verification was interrupted"
	manifestReferencesNotPortable materialManifestRefusal = "material consumer references are not portable"
	manifestInstallFailed         materialManifestRefusal = "material consumer paths could not be installed"
	manifestNotInstalled          materialManifestRefusal = "material consumer paths are not installed"
	manifestChangedInstalling     materialManifestRefusal = "material manifest changed during installation"
	manifestChangedInstalled      materialManifestRefusal = "material manifest changed during installed check"
)

// refuseMaterialManifest is the only place the manifest receiver answers 503.
// It withdraws a staged receipt and labels the answer when the check stopped
// before it reached a verdict: the request or its progress stream was
// interrupted, or the first read or the final recheck lost its database
// leader. The sender then takes this member as unavailable. Every finding
// about bytes, identity, pointer or membership stays unlabelled and remains a
// verdict.
func refuseMaterialManifest(w http.ResponseWriter, reason materialManifestRefusal, cause error) {
	clearMaterialManifestReceipt(w)
	recheck := reason == manifestChangedInstalling || reason == manifestChangedInstalled
	if reason == manifestInterrupted || (recheck && store.ManifestRecheckInterrupted.Unanswered(cause)) ||
		(reason == manifestNotCurrent && store.ManifestReadUnanswered.Unanswered(cause)) {
		w.Header().Set(materialRefusalHeader, materialRefusalInterrupted)
	}
	http.Error(w, string(reason), http.StatusServiceUnavailable)
}
