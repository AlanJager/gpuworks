package hub

// Caps returns the capabilities the agent advertised, as a copy so callers
// cannot mutate session state.
func (s *Session) Caps() []string {
	if len(s.caps) == 0 {
		return nil
	}
	out := make([]string, len(s.caps))
	copy(out, s.caps)
	return out
}

// HasCap reports whether the agent advertised a capability.
func (s *Session) HasCap(name string) bool {
	for _, c := range s.caps {
		if c == name {
			return true
		}
	}
	return false
}
