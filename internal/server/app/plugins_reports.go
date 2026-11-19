package app

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/pluginbridge"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The plugin administration's lifecycle calls and reports, converted from
// the bridge's types to the API's.

func (p pluginAdmin) Restart(ctx context.Context, id, by string) (dto.Plugin, error) {
	v, err := p.b.Restart(ctx, id, by)
	return pluginDTO(v), productErrorOf(err)
}

func (p pluginAdmin) Uninstall(ctx context.Context, id string, purge bool, confirm, by string) (dto.PluginImpact, error) {
	im, err := p.b.Uninstall(ctx, id, pluginbridge.UninstallRequest{Purge: purge, Confirm: extapi.Digest(confirm), By: by})
	return pluginImpactDTO(im), productErrorOf(err)
}

func (p pluginAdmin) Impact(_ context.Context, id string) (dto.PluginImpact, error) {
	im, err := p.b.Impact(id)
	if err != nil {
		return dto.PluginImpact{}, productErrorOf(err)
	}
	return pluginImpactDTO(im), nil
}

func pluginImpactDTO(im pushstate.Impact) dto.PluginImpact {
	out := dto.PluginImpact{ID: im.Owner, Version: im.Version, Confirm: string(im.Digest), Tables: make([]dto.PluginImpactTable, len(im.Tables)),
		Nodes: emptyIfNil(im.Nodes), Features: nonNil(im.Features), Caps: nonNil(im.Caps), Banners: im.Banners, Overrides: emptyIfNil(im.Overrides),
		UsersAffected: im.UsersAffected}
	for i, t := range im.Tables {
		out.Tables[i] = dto.PluginImpactTable{Kind: t.Kind, Entries: t.Entries}
	}
	return out
}

func (p pluginAdmin) State(_ context.Context, id string) (dto.PluginState, error) {
	r, err := p.b.Report(id)
	if err != nil {
		return dto.PluginState{}, productErrorOf(err)
	}
	out := dto.PluginState{ID: r.Owner, Installed: r.Installed, Verified: r.Verified, Now: r.Now.UTC(), GraceS: int64(r.Grace.Seconds()),
		Tables: make([]dto.PluginStateTable, len(r.Tables)), Overrides: make([]dto.PluginOverride, len(r.Overrides))}
	if !r.DownSince.IsZero() {
		since, clamp := r.DownSince.UTC(), r.ClampAt.UTC()
		out.DownSince, out.ClampAt = &since, &clamp
	}
	for i, t := range r.Tables {
		out.Tables[i] = dto.PluginStateTable{Kind: t.Kind, Schema: t.Schema, Generation: t.Generation, Digest: string(t.Digest), Entries: t.Entries,
			PushedAt: t.PushedAt.UTC()}
		if !t.NextStep.IsZero() {
			next := t.NextStep.UTC()
			out.Tables[i].NextStep = &next
		}
	}
	for i, o := range r.Overrides {
		out.Overrides[i] = pluginOverrideDTO(o)
	}
	return out, nil
}

func (p pluginAdmin) Catalog(context.Context) (dto.PluginCatalog, error) {
	c, err := p.b.Catalog()
	if err != nil {
		return dto.PluginCatalog{}, productErrorOf(err)
	}
	out := dto.PluginCatalog{Items: make([]dto.PluginCatalogEntry, len(c.Entries)), Errors: make([]dto.PluginCatalogError, len(c.Errors))}
	for i, e := range c.Entries {
		item := dto.PluginCatalogEntry{ID: e.ID, Publisher: e.Publisher, Installed: e.Installed, Installable: e.Installable,
			Refused: make([]dto.PluginCatalogRefusal, len(e.Refused))}
		if e.Best != nil {
			item.BestVersion, item.MinCoreHint = e.Best.Version, e.Best.MinCoreHint
		}
		for j, r := range e.Refused {
			item.Refused[j] = dto.PluginCatalogRefusal{Version: r.Version, MinCoreHint: r.MinCoreHint,
				Message: dto.ProductMessage{Code: r.Code, Params: r.Params, Message: r.Message}}
		}
		if !e.Installable && len(e.Refused) > 0 {
			first := item.Refused[0].Message
			item.Reason = &first
		}
		out.Items[i] = item
	}
	for i, x := range c.Errors {
		out.Errors[i] = dto.PluginCatalogError{Publisher: x.Publisher, Message: dto.ProductMessage{Code: x.Code, Params: x.Params, Message: x.Message}}
	}
	return out, nil
}

func (p pluginAdmin) Logs(_ context.Context, id string, from api.PluginLogCursor) (api.PluginLogs, <-chan struct{}, error) {
	logs, changed, err := p.b.Logs(id, pluginhost.LogCursor{Event: from.Event, Stderr: from.Stderr})
	if err != nil {
		return api.PluginLogs{}, nil, productErrorOf(err)
	}
	out := api.PluginLogs{Events: make([]dto.PluginEvent, len(logs.Events)), EventsTruncated: logs.EventsTruncated, Stderr: logs.Stderr,
		StderrFrom: logs.StderrFrom, StderrTruncated: logs.StderrTruncated, Next: api.PluginLogCursor{Event: logs.Next.Event, Stderr: logs.Next.Stderr}}
	for i, e := range logs.Events {
		out.Events[i] = dto.PluginEvent{Seq: e.Seq, At: e.At.UTC(), State: string(e.State)}
		if e.Code != "" {
			out.Events[i].Message = &dto.ProductMessage{Code: e.Code, Params: e.Params}
		}
	}
	return out, changed, nil
}

// emptyIfNil is l, or an empty list for nil, so the wire shows [].
func emptyIfNil[T any](l []T) []T {
	if l == nil {
		return []T{}
	}
	return l
}
