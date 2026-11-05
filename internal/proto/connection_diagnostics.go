package proto

// ConnectionValue reads an unambiguous primitive fact. Missing or repeated
// names are unknown, even when repeated values happen to agree.
func ConnectionValue(report DoctorReport, name string) string {
	value := ""
	found := false
	for _, fact := range report.Facts {
		if fact.Code != name {
			continue
		}
		if found {
			return ""
		}
		value, found = fact.Value, true
	}
	return value
}
