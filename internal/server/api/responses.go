package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError answers err as the API does: a product error with its stable
// code and params, any other error with its text. It is for a handler in
// front of the router (the experience environment's guard), so a refusal
// there reads like one from the API.
func WriteError(w http.ResponseWriter, status int, err error) { writeErr(w, status, err) }

func writeErr(w http.ResponseWriter, code int, err error) {
	response := map[string]any{"error": err.Error()}
	var productErr *messages.ProductError
	if errors.As(err, &productErr) {
		response["code"] = productErr.Code()
		w.Header().Set(usageErrorHeader, productErr.Code())
		if params := productErr.Params(); len(params) > 0 {
			response["params"] = params
		}
		noteProductError(w, productErr.Code(), productErr.Params())
	}
	writeJSON(w, code, response)
}

// productErrorNoter is a response writer that records the product error a
// response carries: the server's access log, which writes the code and its
// parameters on the request's line, so a refusal's reason outlives the
// response the client may have dismissed.
type productErrorNoter interface {
	NoteProductError(code string, params map[string]any)
}

// noteProductError hands code and params to the first writer in w's wrapping
// chain that records them.
func noteProductError(w http.ResponseWriter, code string, params map[string]any) {
	for w != nil {
		if noter, ok := w.(productErrorNoter); ok {
			noter.NoteProductError(code, params)
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}
