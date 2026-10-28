package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// aiConfigTestTimeout bounds a connection test: a wrong address must be
// reported while the operator still watches the dialog, not after the
// assistant's full call timeout.
const aiConfigTestTimeout = 10 * time.Second

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

// testAIConfig asks an AI service whether it answers
// (POST /api/v1/ai/configs/test) with the values a form holds, before they are
// saved. It makes the call the assistant makes, through the same outbound
// policy, so its answer is the assistant's answer. Nothing is stored.
func (a *apiHandlers) testAIConfig(w http.ResponseWriter, r *http.Request) {
	var req dto.AIConfigTest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	base, ok := validateAIBaseURL(w, req.BaseURL, true)
	if !ok {
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeFieldInvalid(w, "model", "is required")
		return
	}
	if !validateAIColumn(w, "model", model, 64) {
		return
	}
	key, ok := a.aiConfigTestKey(w, req, base)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), aiConfigTestTimeout)
	defer cancel()
	result := aihelper.NewClient(
		aihelper.Endpoint{BaseURL: base, APIKey: key, Model: model},
		a.d.Outside.AllowPrivateHosts...,
	).Probe(ctx)
	writeJSON(w, http.StatusOK, dto.AIConfigTestResult{
		Outcome: string(result.Outcome), Status: result.Status, Detail: result.Detail,
		BaseURL: base, LatencyMS: result.LatencyMS,
	})
}

// aiConfigTestKey picks the key a test uses. The edit dialog leaves the key
// blank to mean "keep the stored one", and the stored one is used only while
// the address is still the stored address: a test must not be a way to send a
// stored secret to an address of the caller's choosing.
func (a *apiHandlers) aiConfigTestKey(w http.ResponseWriter, req dto.AIConfigTest, base string) (string, bool) {
	if key := strings.TrimSpace(req.APIKey); key != "" || req.ID == 0 {
		return key, true
	}
	stored, err := a.d.Store.AIConfigByID(req.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", true
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return "", false
	}
	if stored.BaseURL != base {
		return "", true
	}
	key, err := store.DecryptAIKey(a.d.AISecretbox, stored.APIKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return "", false
	}
	return key, true
}

// aiConfigView is the public form of a stored service: what the console shows,
// and never the key.
func aiConfigView(c *store.AIConfig) dto.AIConfigView {
	return dto.AIConfigView{ID: c.ID, Name: c.Name, Preset: c.Preset, BaseURL: c.BaseURL, Model: c.Model, IsDefault: c.IsDefault}
}

// writeAIStoreErr answers a config write: a missing service is a 404, a patch
// that would leave the stored services with no default is a 409 - the request
// is well formed and the row exists, the state it asks for is what conflicts -
// and anything else a storage failure.
func writeAIStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if errors.Is(err, store.ErrAIDefaultRequired) {
		writeProductError(w, http.StatusConflict, ErrorAIDefaultRequired,
			"this is the AI service the assistant uses; make another service the default first, or delete this one",
			map[string]any{"field": "is_default", "detail": "the service the assistant uses cannot stop being the default"})
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
