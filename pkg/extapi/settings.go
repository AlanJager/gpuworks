package extapi

import "strconv"

// Setting types.
const (
	SettingString  = "string"
	SettingInteger = "integer"
	SettingBoolean = "boolean"
	SettingEnum    = "enum" // one of Values
)

// Setting limits.
const (
	MaxSettings             = 64
	DefaultSettingMaxLength = 4096
	MaxSettingMaxLength     = 65536
	MaxSettingValues        = 64 // values of one enum setting
)

// SettingDecl declares one value an administrator configures for the
// package. The server validates a value against it before storing it and
// hands the stored values to the plugin; a manifest with settings requires
// host.config.
type SettingDecl struct {
	// Key names the setting, a lower_snake word unique in the manifest.
	Key string `json:"key"`
	// Type is string, integer, boolean or enum.
	Type string `json:"type"`
	// LabelKey is the locale key of the setting's label.
	LabelKey string `json:"label_key"`
	// Required refuses a configuration without a value for the setting.
	Required bool `json:"required,omitempty"`
	// Secret marks a string the server stores sealed, never shows back and
	// hands only to this plugin.
	Secret bool `json:"secret,omitempty"`
	// Values are an enum's allowed values, lower_snake words, sorted.
	Values []string `json:"values,omitempty"`
	// Min and Max bound an integer, inclusive; nil is unbounded.
	Min *int64 `json:"min,omitempty"`
	Max *int64 `json:"max,omitempty"`
	// MaxLength bounds a string in bytes; 0 is DefaultSettingMaxLength.
	MaxLength int `json:"max_length,omitempty"`
}

func validateSettings(settings []SettingDecl) *fieldError {
	if f := inRange("settings", int64(len(settings)), 0, MaxSettings); f != nil {
		return f
	}
	seen := map[string]bool{}
	for i, s := range settings {
		at := joinPath("settings", strconv.Itoa(i))
		if f := first(
			matches(joinPath(at, "key"), s.Key, wordPattern),
			oneOf(joinPath(at, "type"), s.Type, SettingString, SettingInteger, SettingBoolean, SettingEnum),
			matches(joinPath(at, "label_key"), s.LabelKey, localeKeyPattern),
			s.validateTyped(at),
		); f != nil {
			return f
		}
		if seen[s.Key] {
			return &fieldError{field: joinPath(at, "key"), rule: RuleDuplicate}
		}
		seen[s.Key] = true
	}
	return nil
}

// validateTyped checks the fields that belong to one type only.
func (s SettingDecl) validateTyped(at string) *fieldError {
	isString, isInteger, isEnum := s.Type == SettingString, s.Type == SettingInteger, s.Type == SettingEnum
	switch {
	case s.Secret && !isString:
		return &fieldError{field: joinPath(at, "secret"), rule: RuleRelation}
	case s.MaxLength != 0 && !isString:
		return &fieldError{field: joinPath(at, "max_length"), rule: RuleRelation}
	case (s.Min != nil || s.Max != nil) && !isInteger:
		field := "min"
		if s.Min == nil {
			field = "max"
		}
		return &fieldError{field: joinPath(at, field), rule: RuleRelation}
	case len(s.Values) > 0 && !isEnum:
		return &fieldError{field: joinPath(at, "values"), rule: RuleRelation}
	case isEnum && len(s.Values) == 0:
		return &fieldError{field: joinPath(at, "values"), rule: RuleRequired}
	}
	if f := first(
		inRange(joinPath(at, "max_length"), int64(s.MaxLength), 0, MaxSettingMaxLength),
		inRange(joinPath(at, "values"), int64(len(s.Values)), 0, MaxSettingValues),
		checkSortedSet(joinPath(at, "values"), s.Values, wordPattern),
	); f != nil {
		return f
	}
	if s.Min != nil {
		if f := inRange(joinPath(at, "min"), *s.Min, -maxSafeInteger, maxSafeInteger); f != nil {
			return f
		}
	}
	if s.Max != nil {
		lo := int64(-maxSafeInteger)
		if s.Min != nil {
			lo = *s.Min
		}
		if f := inRange(joinPath(at, "max"), *s.Max, lo, maxSafeInteger); f != nil {
			return f
		}
	}
	return nil
}
