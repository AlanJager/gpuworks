package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// updateAIConfig changes one AI service in place (PATCH /api/v1/ai/configs/{id}).
// Partial by design: the edit dialog holds only the fields it shows, and the
// key it never received must survive an edit of the name.
func (a *apiHandlers) updateAIConfig(w http.ResponseWriter, r *http.Request) {
	var req dto.AIConfigPatch
	if !decodeJSONBody(w, r, &req) {
		return
	}
	patch := store.AIConfigPatch{}
	if req.Name != nil {
		if !validateName(w, *req.Name) {
			return
		}
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeFieldInvalid(w, "name", "is required")
			return
		}
		if !validateAIColumn(w, "name", name, 64) {
			return
		}
		patch.Name = &name
	}
	if req.Preset != nil {
		preset := strings.TrimSpace(*req.Preset)
		if !validateAIColumn(w, "preset", preset, 32) {
			return
		}
		patch.Preset = &preset
	}
	if req.BaseURL != nil {
		base, ok := validateAIBaseURL(w, *req.BaseURL, true)
		if !ok {
			return
		}
		patch.BaseURL = &base
	}
	if req.Model != nil {
		model := strings.TrimSpace(*req.Model)
		if model == "" {
			writeFieldInvalid(w, "model", "is required")
			return
		}
		if !validateAIColumn(w, "model", model, 64) {
			return
		}
		patch.Model = &model
	}
	if req.APIKey != nil {
		// An empty key keeps whatever is stored, a legacy plaintext value
		// included: the dialog never received the key and so cannot send it
		// back.
		if key := strings.TrimSpace(*req.APIKey); key != "" {
			encrypted, err := store.EncryptAIKey(a.d.AISecretbox, key)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			patch.APIKey = &encrypted
		}
	}
	patch.IsDefault = req.IsDefault
	c, err := a.d.Store.UpdateAIConfig(parseUint(chi.URLParam(r, "id")), patch)
	if err != nil {
		writeAIStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, aiConfigView(c))
}

// aiConfigView is the public form of a stored service: what the console shows,
// and never the key.
func aiConfigView(c *store.AIConfig) dto.AIConfigView {
	return dto.AIConfigView{ID: c.ID, Name: c.Name, Preset: c.Preset, BaseURL: c.BaseURL, Model: c.Model, IsDefault: c.IsDefault}
}

// writeAIStoreErr answers a config write: a missing service is a 404, anything
// else a storage failure.
func writeAIStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

// writeFieldInvalid rejects the value of one field, named so the console can
// mark the input instead of showing a banner.
func writeFieldInvalid(w http.ResponseWriter, field, detail string) {
	writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, field+" "+detail, map[string]any{"field": field, "detail": detail})
}

// validateAIColumn bounds one stored text field of an AI service by both its
// character limit and its column width.
func validateAIColumn(w http.ResponseWriter, field, value string, maximum int) bool {
	if !validateTextField(w, field, value, maximum) {
		return false
	}
	if len(value) > maximum {
		writeFieldInvalid(w, field, "must be at most "+strconv.Itoa(maximum)+" characters")
		return false
	}
	return true
}

// validateAIBaseURL normalizes an endpoint address and rejects one the client
// could not call. The platform stores base addresses, so a pasted call path is
// dropped (aihelper.NormalizeBaseURL); what remains must be an absolute
// http(s) address with a host.
//
// required is true where a missing address is the caller's error: an edit and
// a connection test both name the address they mean. A create may omit it - a
// preset fills it in, and a service stored without one is only a configuration
// the assistant reports as unusable.
func validateAIBaseURL(w http.ResponseWriter, raw string, required bool) (string, bool) {
	base, _ := aihelper.NormalizeBaseURL(raw)
	if base == "" {
		if !required {
			return "", true
		}
		writeFieldInvalid(w, "base_url", "is required")
		return "", false
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeFieldInvalid(w, "base_url", "must be an http or https address such as https://api.deepseek.com/v1")
		return "", false
	}
	if !validateAIColumn(w, "base_url", base, 255) {
		return "", false
	}
	return base, true
}
