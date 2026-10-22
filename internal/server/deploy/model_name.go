package deploy

// EngineServedName preserves an explicitly cleared alias while older
// deployments continue to use their public routing name.
func (p Params) EngineServedName() string {
	if p.Quick.ServedModelName != nil {
		return *p.Quick.ServedModelName
	}
	return p.ServedName
}
