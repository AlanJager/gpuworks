package modelmetadata

import (
	"errors"
	"fmt"
	"strings"
)

// ValidateSingleEntry is shared by node registration and server import so that
// a failed recognition retains the parser's diagnosis across both entrypoints.
func (r Report) ValidateSingleEntry() error {
	var reason string
	switch {
	case r.Component && len(r.Entries) == 0:
		reason = "directory is a model component"
	case len(r.Entries) == 0:
		reason = "directory must identify exactly one model entry (found 0): no supported model metadata and weights were recognized"
	case len(r.Entries) != 1:
		reason = fmt.Sprintf("directory must identify exactly one model entry (found %d); select a single model directory", len(r.Entries))
	case r.Entries[0].Incomplete:
		reason = "model entry has missing or unresolved files"
	default:
		return nil
	}
	var details []string
	if len(r.Entries) == 1 {
		details = append(details, r.Entries[0].Missing...)
	}
	for _, issue := range r.Issues {
		if issue.Code != "unknown_task" {
			details = append(details, strings.TrimSpace(issue.Path+": "+issue.Code+" "+issue.Detail))
		}
	}
	if len(details) > 0 {
		reason += ": " + strings.Join(details, "; ")
	}
	return errors.New(reason)
}
