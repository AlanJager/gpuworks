package api

func (a *apiHandlers) pruneUnreferencedCatalogPackages() {
	if a == nil || a.d.Store == nil || a.d.CatalogPackages == nil {
		return
	}
	referenced, err := a.d.Store.ReferencedCatalogPackageDigests()
	if err != nil {
		if a.d.Log != nil {
			a.d.Log.Warn("list referenced inference catalog packages", "error", err)
		}
		return
	}
	removed, err := a.d.CatalogPackages.Prune(referenced)
	if err != nil {
		if a.d.Log != nil {
			a.d.Log.Warn("prune inference catalog packages", "error", err)
		}
		return
	}
	if len(removed) > 0 && a.d.Log != nil {
		a.d.Log.Info("pruned unreferenced inference catalog packages", "count", len(removed))
	}
}
