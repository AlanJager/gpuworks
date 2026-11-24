package api

import (
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
)

// consoleManifest answers GET /console/manifest: the edition, the declared
// features' availability, the extensions' menu entries the viewer's features
// leave visible, and the banners for this viewer.
func (a *apiHandlers) consoleManifest(w http.ResponseWriter, r *http.Request) {
	ext := a.d.Extensions
	out := dto.ConsoleManifest{Edition: ext.Edition, Features: []dto.ConsoleFeature{}, Nav: []dto.NavContribution{}, Banners: []dto.ConsoleBanner{}}
	if out.Edition == "" {
		out.Edition = "community"
	}
	for _, s := range ext.Features.States() {
		out.Features = append(out.Features, consoleFeature(s))
	}
	for _, n := range a.extensionNav {
		if n.Feature != "" && ext.Features.Access(featuregate.Key(n.Feature)) == featuregate.Hidden {
			continue
		}
		out.Nav = append(out.Nav, n)
	}
	claims, _ := a.claimsFromRequest(r)
	for _, b := range ext.Banners {
		for _, banner := range b.Banners(r.Context(), claims) {
			if banner.ID == "" || banner.Message.Code == "" {
				continue
			}
			switch banner.Severity {
			case "info", "warning", "danger":
			default:
				banner.Severity = "info"
			}
			out.Banners = append(out.Banners, banner)
		}
	}
	if limit := a.consolePlatform(); limit != nil {
		out.Platform = &dto.ConsolePlatform{Access: limit.Access.String(),
			Message: dto.ProductMessage{Code: limit.Notice.Code(), Params: limit.Notice.Params(), Message: limit.Notice.Error()}}
		// The page that lifts the limit is an administrator's; anyone else
		// is told of the limit and offered nothing to open.
		if claims.Role == "admin" {
			out.Platform.Link = limit.Link
		}
		if limit.Action != nil && out.Platform.Link != "" {
			out.Platform.Action = &dto.ProductMessage{Code: limit.Action.Code(), Params: limit.Action.Params(), Message: limit.Action.Error()}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func consoleFeature(s featuregate.State) dto.ConsoleFeature {
	f := dto.ConsoleFeature{Key: string(s.Key), Access: s.Access.String(), Presentation: s.Presentation.String(), Limits: []dto.FeatureLimit{}}
	if s.Access != featuregate.Full {
		f.Link = s.Link
	}
	if s.Notice != nil {
		f.Message = &dto.ProductMessage{Code: s.Notice.Code(), Params: s.Notice.Params(), Message: s.Notice.Error()}
	}
	for _, l := range s.Limits {
		limit := dto.FeatureLimit{Dim: l.Dim}
		if !l.Unlimited {
			limit.Max = &l.Max
		}
		f.Limits = append(f.Limits, limit)
	}
	return f
}
