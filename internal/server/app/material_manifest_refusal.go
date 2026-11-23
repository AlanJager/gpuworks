//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"net/http"

	"gorm.io/gorm"

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
	unread := reason == manifestNotCurrent || reason == manifestChangedInstalling || reason == manifestChangedInstalled
	if reason == manifestInterrupted || (unread && materialVerificationInterrupted(cause)) {
		w.Header().Set(materialRefusalHeader, materialRefusalInterrupted)
	}
	http.Error(w, string(reason), http.StatusServiceUnavailable)
}

// materialVerificationInterrupted reports a recheck that failed only because
// the database or the request went away under it: no elected leader, a
// connection the driver or the network dropped, or an expired or cancelled
// context. A definitive refusal or a missing row wins even when such an error
// is joined to it, and an error of unknown kind is a verdict.
func materialVerificationInterrupted(err error) bool {
	if err == nil {
		return false
	}
	if branches, joined := materialJoinedBranches(err); joined {
		return everyMaterialBranch(branches, materialVerificationInterrupted)
	}
	switch {
	case errors.Is(err, store.ErrClusterAdmissionRejected),
		errors.Is(err, store.ErrClusterIdentityConflict),
		errors.Is(err, store.ErrMaintenanceNotServing),
		errors.Is(err, store.ErrBusinessWritesClosed),
		errors.Is(err, gorm.ErrRecordNotFound):
		return false
	}
	var network net.Error
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		store.ReplicatedLeaderUnavailable(err) ||
		errors.Is(err, store.ErrClusterMembershipObservationTimeout) ||
		errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.As(err, &network)
}
