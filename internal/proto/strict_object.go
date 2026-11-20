package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// decodeModelDownloadControl accepts one JSON object whose keys are exactly
// fields. Unknown, duplicate, null, and trailing values are rejected.
func decodeModelDownloadControl(raw json.RawMessage, fields map[string]any) error {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return errors.New("invalid model download control size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return errors.New("model download control requires an object")
	}
	seen := make(map[string]bool, len(fields))
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("invalid model download control key")
		}
		dest, ok := fields[key]
		if !ok || seen[key] {
			return errors.New("unknown or duplicate model download control field")
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("null model download control field")
		}
		if err := json.Unmarshal(value, dest); err != nil {
			return err
		}
	}
	if token, err := d.Token(); err != nil {
		return err
	} else if token != json.Delim('}') {
		return errors.New("invalid model download control object")
	}
	if len(seen) != len(fields) {
		return errors.New("missing model download control field")
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing model download control JSON")
	}
	return nil
}
