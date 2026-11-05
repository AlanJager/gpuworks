package modelmetadata

import (
	"errors"
	"fmt"
	"slices"
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
		reason = "directory must identify exactly one model entry (found 0)"
	case len(r.Entries) != 1:
		reason = fmt.Sprintf("directory must identify exactly one model entry (found %d); select a single model directory", len(r.Entries))
	case r.Entries[0].Incomplete:
		reason = "model entry has missing or unresolved files"
	default:
		return nil
	}
	var details []string
	add := func(detail string) {
		if detail = strings.TrimSpace(detail); detail != "" && !slices.Contains(details, detail) {
			details = append(details, detail)
		}
	}
	if len(r.Entries) == 1 {
		for _, name := range r.Entries[0].Missing {
			add("missing or unresolved file: " + name)
		}
	} else if len(r.Entries) > 1 {
		for _, entry := range r.Entries {
			name := entry.Path
			if len(entry.Files) > 0 {
				name = entry.Files[0]
			}
			add(fmt.Sprintf("%s entry: %s", entry.Format, name))
		}
	}
	for _, issue := range r.Issues {
		if issue.Code != "unknown_task" {
			detail := issue.Detail
			if detail == "" {
				detail = strings.ReplaceAll(issue.Code, "_", " ")
			}
			if issue.Path != "" && issue.Path != "." {
				detail = issue.Path + ": " + detail
			}
			add(detail)
		}
	}
	if len(details) > 0 {
		reason += ":\n- " + strings.Join(details, "\n- ")
	} else if len(r.Entries) == 0 && !r.Component {
		reason += ": the recognizer returned no detailed failure reason"
	}
	return errors.New(reason)
}
