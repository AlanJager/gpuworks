package modelmetadata

// A rule can explain why its evidence did not identify an entry without
// declaring that evidence corrupt. Other formats may still recognize it.
const unrecognizedModelIssue = "unrecognized_model"

func unrecognized(name, reason string) RuleResult {
	return RuleResult{Issues: []Issue{{Path: name, Code: unrecognizedModelIssue, Detail: reason}}}
}

// HasMetadataIssues distinguishes malformed/unreadable metadata from a valid
// directory that no format recognized. Scanning keeps its existing layout
// classification for the latter while registration can explain the rejection.
func (r Report) HasMetadataIssues() bool {
	for _, issue := range r.Issues {
		if issue.Code != unrecognizedModelIssue {
			return true
		}
	}
	return false
}

func recognitionIssues(issues []Issue, recognized bool) []Issue {
	var out []Issue
	seen := map[Issue]bool{}
	for _, issue := range issues {
		if recognized && issue.Code == unrecognizedModelIssue {
			continue
		}
		key := issue
		// The same document may be read by several formats and task inference.
		// A file and its concrete cause identify one failure, whatever reader
		// reported it first. Code-only diagnostics retain their code identity.
		if key.Detail != "" {
			key.Code = ""
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, issue)
		}
	}
	return out
}
