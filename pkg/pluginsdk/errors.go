package pluginsdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Error is a gwx/1 error: a refusal the plugin answers the server with, or
// one the server's host API answered the plugin with. Status is the HTTP
// status it travelled under.
type Error struct {
	Status int
	Wire   extapi.WireError
}

func (e *Error) Error() string {
	return fmt.Sprintf("gwx error %d %s: %s", e.Status, e.Wire.Code, e.Wire.Message)
}

// Refuse builds a refusal with code (one of the package's declared codes,
// or an extapi code), its English fallback message, params and reason.
func Refuse(reason, code, message string, params extapi.Params) *Error {
	return &Error{
		Status: extapi.StatusForReason(reason),
		Wire:   extapi.WireError{Code: code, Message: message, Params: params, Reason: reason},
	}
}

// refusalOf turns an extapi decoding or validation failure into a refusal.
func refusalOf(err error, reason string) *Error {
	var e *extapi.Error
	if !errors.As(err, &e) {
		e = &extapi.Error{Code: extapi.ErrorWireMalformed, Message: err.Error(),
			Params: map[string]any{"object": "request", "field": "_root", "rule": extapi.RuleSyntax}}
	}
	w := extapi.WireErrorOf(e, reason)
	w.Message = clipMessage(w.Message)
	return &Error{Status: extapi.StatusForReason(reason), Wire: w}
}

// writeError answers with e's wire body; an invalid body (a plugin that
// built a refusal with a bad code) is answered as an internal failure in
// the plugin's own words, never left unanswered.
func writeError(w http.ResponseWriter, e *Error) {
	wire := e.Wire
	if err := wire.Validate(); err != nil {
		wire = extapi.WireErrorOf(&extapi.Error{Code: extapi.ErrorWireInvalid, Message: err.Error(),
			Params: map[string]any{"object": "error", "field": "_root", "rule": extapi.RuleRelation}}, extapi.ReasonInternal)
	}
	status := e.Status
	if status < 400 || status > 599 {
		status = extapi.StatusForReason(wire.Reason)
	}
	body, err := extapi.Marshal(wire)
	if err != nil {
		http.Error(w, "unencodable error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", extapi.ContentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeJSON answers 200 with v in canonical form.
func writeJSON(w http.ResponseWriter, v any) {
	body, err := extapi.Marshal(v)
	if err != nil {
		writeError(w, Refuse(extapi.ReasonInternal, extapi.ErrorWireInvalid, err.Error(),
			extapi.Params{"object": "response", "field": "_root", "rule": extapi.RuleType}))
		return
	}
	w.Header().Set("Content-Type", extapi.ContentTypeJSON)
	_, _ = w.Write(body)
}

// maxCallBody bounds every lifecycle body either side reads.
const maxCallBody = 1 << 20

// readError decodes a failed response's body as a gwx/1 error. A body that
// is not one is reported as such, with the status.
func readError(resp *http.Response) error {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCallBody))
	if err != nil {
		return fmt.Errorf("gwx: status %d, reading the error body: %w", resp.StatusCode, err)
	}
	wire, derr := extapi.DecodeWireError(raw)
	if derr != nil {
		return fmt.Errorf("gwx: status %d with a body that is not an error object: %w", resp.StatusCode, derr)
	}
	return &Error{Status: resp.StatusCode, Wire: *wire}
}

// decodeBody reads a bounded JSON body into a value with a strict decoder
// of its own (DecodeStateAck, DecodeHandshakeRequest, ...).
func readBody(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxCallBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCallBody {
		return nil, &extapi.Error{Code: extapi.ErrorWireMalformed, Message: "body too large",
			Params: map[string]any{"object": "request", "field": "_root", "rule": extapi.RuleRange}}
	}
	return raw, nil
}

// compactJSON is used only in logs.
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// clipMessage bounds an English fallback to extapi.MaxErrorMessage bytes,
// cut at a rune boundary so the message stays valid UTF-8.
func clipMessage(s string) string {
	if len(s) <= extapi.MaxErrorMessage {
		return s
	}
	n := extapi.MaxErrorMessage
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
