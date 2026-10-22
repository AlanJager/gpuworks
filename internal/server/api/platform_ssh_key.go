package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// platformSSHKeyResponse never carries the private key.
type platformSSHKeyResponse struct {
	PublicKey        string    `json:"public_key"`
	Fingerprint      string    `json:"fingerprint"`
	CreatedAt        time.Time `json:"created_at"`
	AuthorizeCommand string    `json:"authorize_command"`
}

func platformSSHKeyDTO(row store.PlatformSSHKey) platformSSHKeyResponse {
	return platformSSHKeyResponse{
		PublicKey: row.PublicKey, Fingerprint: row.Fingerprint, CreatedAt: row.CreatedAt,
		AuthorizeCommand: provision.AuthorizeKeyCommand(row.PublicKey),
	}
}

// platformKey loads the server's SSH key for a login, or nil when there is
// none (generation failed at startup); callers then try only what the
// request carries.
func (a *apiHandlers) platformKey() *provision.PlatformKey {
	if a.d.Store == nil {
		return nil
	}
	row, pem, err := a.d.Store.PlatformSSHKey()
	if err != nil {
		return nil
	}
	return &provision.PlatformKey{PrivatePEM: pem, PublicKey: row.PublicKey, Fingerprint: row.Fingerprint}
}

func (a *apiHandlers) getPlatformSSHKey(w http.ResponseWriter, r *http.Request) {
	row, _, err := a.d.Store.PlatformSSHKey()
	if errors.Is(err, store.ErrNotFound) {
		writeProductError(w, http.StatusNotFound, ErrorPlatformSSHKeyMissing, "the platform SSH key has not been generated", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, platformSSHKeyDTO(row))
}

// rotatePlatformSSHKey replaces the key with a new one. Targets that only
// authorized the old public key stop accepting the server until the new one
// is authorized; the UI says so before calling this.
func (a *apiHandlers) rotatePlatformSSHKey(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	key, err := provision.GeneratePlatformKey(provision.PlatformKeyComment(hostname))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.savePlatformSSHKey(w, key)
}

// importPlatformSSHKey makes an existing, unencrypted private key the
// platform key, for sites whose nodes already trust a key of their own.
func (a *apiHandlers) importPlatformSSHKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PrivateKeyPEM string `json:"private_key_pem"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hostname, _ := os.Hostname()
	key, err := provision.ParsePlatformKey(req.PrivateKeyPEM, provision.PlatformKeyComment(hostname))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	a.savePlatformSSHKey(w, key)
}

func (a *apiHandlers) savePlatformSSHKey(w http.ResponseWriter, key provision.PlatformKey) {
	row, err := a.d.Store.SetPlatformSSHKey(key.PrivatePEM, key.PublicKey, key.Fingerprint)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, platformSSHKeyDTO(row))
}
