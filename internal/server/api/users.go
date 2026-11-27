package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Accounts and external identities (ADR 0085, step 1). Administrators manage
// accounts; an identity connector, once step 2 lands, links and disables
// through the store underneath these routes.

func userDTO(u store.User) dto.User {
	return dto.User{
		ID: u.ID, Username: u.Username, Role: u.Role, LocalAdmin: u.LocalAdmin,
		Disabled: u.DisabledAt != nil, DisabledAt: u.DisabledAt,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func externalIdentityDTO(i store.UserIdentity) dto.ExternalIdentity {
	return dto.ExternalIdentity{
		ID: i.ID, Source: i.Source, Subject: i.Subject,
		CreatedAt: i.CreatedAt, LastSignInAt: i.LastSignInAt,
	}
}

// listUsers answers every account, disabled ones included: an administrator
// manages accounts, and a console has to show what it has disabled.
func (a *apiHandlers) listUsers(w http.ResponseWriter, r *http.Request) {
	if a.d.Store == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorStoreUnavailable, "the account store is not available", nil)
		return
	}
	rows, err := a.d.Store.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	items := make([]dto.User, 0, len(rows))
	for _, row := range rows {
		items = append(items, userDTO(row))
	}
	writeJSON(w, http.StatusOK, dto.NewList(items))
}

// disableUser disables an account: its sessions end at once, and its sign-in,
// platform tokens and inference API keys are refused. The last enabled local
// administrator cannot be disabled (ADR 0071 decision 11), which also keeps an
// administrator from locking themselves out by disabling their own account.
func (a *apiHandlers) disableUser(w http.ResponseWriter, r *http.Request) {
	a.setUserDisabled(w, r, true)
}

// enableUser restores a disabled account; its data and keys work again.
func (a *apiHandlers) enableUser(w http.ResponseWriter, r *http.Request) {
	a.setUserDisabled(w, r, false)
}

func (a *apiHandlers) setUserDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	id, ok := a.userIDFromPath(w, r)
	if !ok {
		return
	}
	var (
		u   *store.User
		err error
	)
	if disabled {
		u, err = a.d.Store.DisableUser(id)
	} else {
		u, err = a.d.Store.EnableUser(id)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProductError(w, http.StatusNotFound, ErrorUserNotFound, "no account has this id", map[string]any{"id": id})
		return
	case errors.Is(err, store.ErrLastLocalAdmin):
		writeProductError(w, http.StatusForbidden, ErrorLastLocalAdmin,
			"this is the last enabled local administrator; the platform needs one to stay reachable", nil)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, userDTO(*u))
}

// listUserIdentities answers the account's links to outside identity sources.
func (a *apiHandlers) listUserIdentities(w http.ResponseWriter, r *http.Request) {
	id, ok := a.userIDFromPath(w, r)
	if !ok {
		return
	}
	if _, err := a.d.Store.UserByID(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProductError(w, http.StatusNotFound, ErrorUserNotFound, "no account has this id", map[string]any{"id": id})
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rows, err := a.d.Store.ExternalIdentities(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	items := make([]dto.ExternalIdentity, 0, len(rows))
	for _, row := range rows {
		items = append(items, externalIdentityDTO(row))
	}
	writeJSON(w, http.StatusOK, dto.NewList(items))
}

// linkUserIdentity links an existing account to (source, subject). It never
// creates an account and never changes its role: an identity connector cannot
// make anyone an administrator (ADR 0085 decision 5).
func (a *apiHandlers) linkUserIdentity(w http.ResponseWriter, r *http.Request) {
	id, ok := a.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req dto.ExternalIdentityRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	row, err := a.d.Store.LinkIdentity(id, req.Source, req.Subject)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProductError(w, http.StatusNotFound, ErrorUserNotFound, "no account has this id", map[string]any{"id": id})
		return
	case errors.Is(err, store.ErrInvalidIdentity):
		writeProductError(w, http.StatusBadRequest, ErrorExternalIdentityInvalid,
			"source must match [a-z][a-z0-9_]{1,31} and subject must be 1 to 256 bytes", map[string]any{"max_subject": 256})
		return
	case errors.Is(err, store.ErrIdentityTaken):
		writeProductError(w, http.StatusConflict, ErrorExternalIdentityTaken,
			"this external identity is already linked to an account", map[string]any{"source": req.Source, "subject": req.Subject})
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, externalIdentityDTO(*row))
}

// unlinkUserIdentity removes one of the account's links; a link of another
// account answers 404, so an id cannot be taken away by guessing it.
func (a *apiHandlers) unlinkUserIdentity(w http.ResponseWriter, r *http.Request) {
	id, ok := a.userIDFromPath(w, r)
	if !ok {
		return
	}
	identityID, ok := a.uintPathParam(w, r, "identityID")
	if !ok {
		return
	}
	err := a.d.Store.UnlinkIdentity(id, identityID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProductError(w, http.StatusNotFound, ErrorExternalIdentityNotFound,
			"no external identity of this account has this id", map[string]any{"id": identityID})
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiHandlers) userIDFromPath(w http.ResponseWriter, r *http.Request) (uint, bool) {
	if a.d.Store == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorStoreUnavailable, "the account store is not available", nil)
		return 0, false
	}
	return a.uintPathParam(w, r, "id")
}

// uintPathParam parses a numeric path parameter; a value that is not a
// positive integer names no resource, so it answers 404 like a missing one.
func (a *apiHandlers) uintPathParam(w http.ResponseWriter, r *http.Request, name string) (uint, bool) {
	raw := chi.URLParam(r, name)
	var n uint64
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			n = 0
			break
		}
		n = n*10 + uint64(raw[i]-'0')
		if n > 1<<32 {
			n = 0
			break
		}
	}
	if n == 0 {
		writeProductError(w, http.StatusNotFound, ErrorUserNotFound, "no resource has this id", map[string]any{"id": raw})
		return 0, false
	}
	return uint(n), true
}
