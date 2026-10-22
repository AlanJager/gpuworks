package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Logins for the hosts nodes fetch from, one per host: a registry's user and
// password, or a model site's token. Nodes hold them in memory from their
// last state sync; the management node uses them for its own reads.

func credentialDTO(view store.SourceCredentialView) dto.Credential {
	return dto.Credential{ID: view.ID, Host: view.Host, Username: view.Username, HasSecret: view.HasSecret}
}

func (a *apiHandlers) listCredentials(w http.ResponseWriter, r *http.Request) {
	views, err := a.d.Store.SourceCredentials()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]dto.Credential, 0, len(views))
	for _, view := range views {
		out = append(out, credentialDTO(view))
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

func (a *apiHandlers) putCredential(w http.ResponseWriter, r *http.Request, id uint, status int) {
	var req dto.CredentialRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	view, err := a.d.Store.PutSourceCredential(id, req.Host, req.Username, req.Secret)
	if err != nil {
		writePullSettingError(w, err)
		return
	}
	a.syncOnlineNodes()
	writeJSON(w, status, credentialDTO(view))
}

func (a *apiHandlers) createCredential(w http.ResponseWriter, r *http.Request) {
	a.putCredential(w, r, 0, http.StatusCreated)
}

func (a *apiHandlers) updateCredential(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	if id == 0 {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	a.putCredential(w, r, id, http.StatusOK)
}

func (a *apiHandlers) deleteCredential(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.DeleteSourceCredential(parseUint(chi.URLParam(r, "id"))); err != nil {
		writePullSettingError(w, err)
		return
	}
	a.syncOnlineNodes()
	w.WriteHeader(http.StatusNoContent)
}

// credentialLoginTimeout bounds a login test: two requests at most.
const credentialLoginTimeout = 20 * time.Second

// testCredential tries a stored login from the management node, through its
// outbound proxy: a registry login against the registry's /v2/, a site token
// against the hub's whoami. A node may reach a host the management node
// cannot; the test says what the management node sees.
func (a *apiHandlers) testCredential(w http.ResponseWriter, r *http.Request) {
	login, err := a.d.Store.SourceAuthFor(parseUint(chi.URLParam(r, "id")))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeProductError(w, http.StatusConflict, ErrorCredentialLoginFailed, err.Error(), map[string]any{"detail": err.Error()})
		return
	}
	transport := a.outboundTransport()
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(r.Context(), credentialLoginTimeout)
	defer cancel()
	client := &http.Client{Transport: transport}
	if login.Username == "" {
		err = modelmeta.CheckHubToken(ctx, client, login.Host, login.Secret)
	} else {
		err = imagecatalog.CheckRegistryLogin(ctx, client, login.Host, login.Username, login.Secret)
	}
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, dto.CredentialLoginTest{OK: true})
	case errors.Is(err, imagecatalog.ErrRegistryLoginRejected), errors.Is(err, modelmeta.ErrTokenRejected):
		writeProductError(w, http.StatusUnprocessableEntity, ErrorCredentialLoginRejected, err.Error(), map[string]any{"host": login.Host})
	default:
		writeProductError(w, http.StatusBadGateway, ErrorCredentialLoginFailed, err.Error(), map[string]any{"host": login.Host, "detail": err.Error()})
	}
}

// registryCredentialsRemoved answers the routes the registry logins had
// before every login moved to /pull/credentials.
func (a *apiHandlers) registryCredentialsRemoved(w http.ResponseWriter, _ *http.Request) {
	writeProductError(w, http.StatusGone, ErrorSettingRemoved,
		"registry credentials moved to /api/v1/pull/credentials, keyed by host",
		map[string]any{"setting": "registry-credentials", "route": "/api/v1/pull/credentials"})
}
