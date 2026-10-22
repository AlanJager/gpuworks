package localization

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
)

// LoadJSONFiles strictly decodes one embedded JSON resource per language.
// filePattern must contain one %s placeholder for the language code.
func LoadJSONFiles[T any](filesystem fs.FS, filePattern string, languages []string) (map[string]T, error) {
	locales := make(map[string]T, len(languages))
	for _, language := range languages {
		path := fmt.Sprintf(filePattern, language)
		raw, err := fs.ReadFile(filesystem, path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", language, err)
		}
		var resource T
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&resource); err != nil {
			return nil, fmt.Errorf("decode %s: %w", language, err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return nil, fmt.Errorf("decode %s: unexpected trailing JSON", language)
		}
		locales[language] = resource
	}
	return locales, nil
}
