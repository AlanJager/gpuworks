package api

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// diagnosticError carries request-scoped explanations without replacing the
// product error or its underlying cause used by errors.Is/As.
type diagnosticError struct {
	error
	report *dto.DiagnosticReport
}

func (e *diagnosticError) Unwrap() error                      { return e.error }
func (e *diagnosticError) Diagnostics() *dto.DiagnosticReport { return e.report }

func withDiagnostics(err error, report *dto.DiagnosticReport) error {
	if err == nil || report == nil || len(report.Checks) == 0 {
		return err
	}
	return &diagnosticError{error: err, report: report}
}

func diagnosticsFromError(err error) *dto.DiagnosticReport {
	var explained interface{ Diagnostics() *dto.DiagnosticReport }
	if errors.As(err, &explained) {
		return explained.Diagnostics()
	}
	return nil
}
