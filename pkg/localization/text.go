// Package localization defines wire types shared by server-owned dictionaries.
// Product text is keyed by a normalized BCP 47 language code and resolved by
// clients using their current locale with an English fallback.
package localization

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const (
	// English is the source and mandatory fallback language.
	English = "en"
	// Chinese is the first supported translation language.
	Chinese = "zh"
)

// Text contains every translation of one server-owned display string.
//
// Keep this as a map on the wire so adding another language does not require a
// DTO or API schema change.
type Text map[string]string

// SameText builds a language map for technical display values such as URLs
// that are intentionally identical in every locale.
func SameText(value string, languages ...string) Text {
	text := make(Text, len(languages))
	for _, language := range languages {
		text[language] = value
	}
	return text
}

// UnmarshalJSON accepts the previous single-string wire/storage form during
// the one-version migration window. New marshaling always emits a language
// map. A legacy value is placed under English so clients reach it through the
// documented fallback path.
func (t *Text) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*t = nil
		return nil
	}
	var legacy string
	if err := json.Unmarshal(data, &legacy); err == nil {
		*t = Text{English: legacy}
		return nil
	}
	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("localized text must be a string or language map: %w", err)
	}
	*t = values
	return nil
}
