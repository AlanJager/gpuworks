package store

// PullSettingError names a refused field and, when known, a language-neutral
// validation reason. Err remains the diagnostic fallback for older clients.
type PullSettingError struct {
	Field  string
	Reason string
	Params map[string]any
	Err    error
}

func (e *PullSettingError) Error() string { return e.Field + ": " + e.Err.Error() }
func (e *PullSettingError) Unwrap() error { return e.Err }

func pullSettingError(field string, err error) error {
	return &PullSettingError{Field: field, Err: err}
}

func pullSettingReasonError(field, reason string, err error, params map[string]any) error {
	return &PullSettingError{Field: field, Reason: reason, Params: params, Err: err}
}
