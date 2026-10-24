package analytics

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
)

// GPU heat map metrics.
const (
	MetricUtil  = "util"
	MetricPower = "power"
	MetricTemp  = "temp"
	MetricMem   = "mem"
)

// GPUQuery is a GPU utilization query: the range, what the rows are, what
// the heat map's colour is, and filters.
type GPUQuery struct {
	Query
	GroupBy string // model | node | gpu
	Metric  string // util | power | temp | mem
	// NodeUUIDs, when not nil, are the only nodes read (the nodes a label
	// filter matched); GPUModel, when not empty, the only card model.
	NodeUUIDs []string
	GPUModel  string
}

// maxDetailCards bounds the cards answered one by one: past it the answer
// holds the models' or nodes' rows only, read from their own statistics.
const maxDetailCards = 256

// maxHeatNodes bounds a node heat map: past it, the half with the highest
// metric and the half with the lowest are shown.
const maxHeatNodes = 100

func gpuFigures(g rollup.GPU) dto.GPUFigures {
	out := dto.GPUFigures{UtilSamples: g.UtilN, PowerSamples: g.PowerN}
	if g.UtilN > 0 {
		out.UtilAvg, out.UtilMax = ptr(g.UtilSum/float64(g.UtilN)), ptr(g.UtilMax)
	}
	if g.MemUsedN > 0 {
		out.MemUsedAvg, out.MemUsedMax = ptr(g.MemUsedSum/float64(g.MemUsedN)), ptr(g.MemUsedMax)
	}
	if g.MemTotal > 0 {
		out.MemTotal = ptr(g.MemTotal)
	}
	if g.PowerN > 0 {
		out.PowerAvg = ptr(g.PowerSum / float64(g.PowerN))
	}
	if g.TempN > 0 {
		out.TempAvg, out.TempMax = ptr(g.TempSum/float64(g.TempN)), ptr(g.TempMax)
	}
	return out
}

// metricValue is a heat map cell: the metric's mean, or nil without samples.
func metricValue(g rollup.GPU, metric string) *float64 {
	switch metric {
	case MetricPower:
		if g.PowerN > 0 {
			return ptr(g.PowerSum / float64(g.PowerN))
		}
	case MetricTemp:
		if g.TempN > 0 {
			return ptr(g.TempSum / float64(g.TempN))
		}
	case MetricMem:
		if g.MemUsedN > 0 {
			return ptr(g.MemUsedSum / float64(g.MemUsedN))
		}
	default:
		if g.UtilN > 0 {
			return ptr(g.UtilSum / float64(g.UtilN))
		}
	}
	return nil
}

// GPUs answers the GPU utilization over a range. The cards the filter
// covers now decide the shape: up to maxDetailCards are answered one by one,
// more only as models' or nodes' rows, which have statistics of their own
// so a fleet's range never reads every card's.
func (s *Service) GPUs(ctx context.Context, q GPUQuery) (dto.GPUAnalytics, error) {
	if q.GroupBy == "" {
		q.GroupBy = string(statsdb.GPUByModel)
	}
	if q.Metric == "" {
		q.Metric = MetricUtil
	}
	by := statsdb.GPUGroupBy(q.GroupBy)
	switch by {
	case statsdb.GPUByModel, statsdb.GPUByNode, statsdb.GPUByCard:
	default:
		return dto.GPUAnalytics{}, queryError(messages.ErrorAnalyticsValueInvalid, "group_by", "model, node or gpu")
	}
	switch q.Metric {
	case MetricUtil, MetricPower, MetricTemp, MetricMem:
	default:
		return dto.GPUAnalytics{}, queryError(messages.ErrorAnalyticsValueInvalid, "metric", "util, power, temp or mem")
	}
	sp, err := s.resolve(q.Query)
	if err != nil {
		return dto.GPUAnalytics{}, err
	}
	sc, err := s.gpuScope(q)
	if err != nil {
		return dto.GPUAnalytics{}, err
	}
	detail := len(sc.cards) <= maxDetailCards
	if by == statsdb.GPUByCard && !detail {
		return dto.GPUAnalytics{}, queryError(messages.ErrorAnalyticsTooManyCards, "group_by",
			fmt.Sprintf("%d cards; filter by node label or model to see at most %d by card", len(sc.cards), maxDetailCards))
	}
	parts, keys, err := s.statsParts(ctx, sp)
	if err != nil {
		return dto.GPUAnalytics{}, err
	}
	f := statsdb.GPUFilter{NodeUUIDs: q.NodeUUIDs, GPUModel: q.GPUModel}
	out := dto.GPUAnalytics{Metric: q.Metric, GroupBy: q.GroupBy, CardCount: len(sc.cards), ByModel: sc.byModel(),
		Detail: detail, Cards: []dto.GPUCardRow{}, Groups: []dto.GPUGroupRow{}, Heat: []dto.GPUHeatRow{}}

	// A few cards of a larger fleet are read on their nodes only, and on
	// the nodes deleted since the range began: their cards count in the
	// groups' rows, so they are listed too.
	cf := f
	if detail && cf.NodeUUIDs == nil && len(sc.cards) < sc.fleet {
		gone, err := s.Store.NodesDeletedSince(time.UnixMilli(sp.fromMS))
		if err != nil {
			return out, err
		}
		cf.NodeUUIDs = append(sc.nodes(), gone...)
	}
	var total rollup.GPU
	if detail {
		cards, err := s.gpuCards(ctx, parts, cf)
		if err != nil {
			return out, err
		}
		nodes := make([]string, 0, len(cards))
		for k := range cards {
			nodes = append(nodes, k.node)
		}
		if err := s.nameNodes(sc, nodes); err != nil {
			return out, err
		}
		for k, c := range cards {
			row := dto.GPUCardRow{NodeUUID: k.node, NodeName: sc.names[k.node], NodeDeleted: sc.deleted[k.node], GPUIndex: k.index, GPUUUID: k.uuid,
				GPUModel: c.GPUModel, Present: sc.present[k], Deployments: sc.holders[k], GPUFigures: gpuFigures(c.GPU)}
			if row.Deployments == nil {
				row.Deployments = []string{}
			}
			out.Cards = append(out.Cards, row)
			total.Add(c.GPU)
		}
		sort.Slice(out.Cards, func(i, j int) bool {
			a, b := out.Cards[i], out.Cards[j]
			if a.NodeName != b.NodeName {
				return a.NodeName < b.NodeName
			}
			return a.GPUIndex < b.GPUIndex
		})
	}
	var groups map[string]rollup.GPU
	if by != statsdb.GPUByCard {
		if groups, err = s.gpuGroups(ctx, parts, f, by); err != nil {
			return out, err
		}
		if !detail {
			for _, g := range groups {
				total.Add(g)
			}
		}
		if by == statsdb.GPUByNode {
			if err := s.nameNodes(sc, slices.Collect(maps.Keys(groups))); err != nil {
				return out, err
			}
		}
		out.Groups = sc.groupRows(by, groups)
	}
	out.Totals = gpuFigures(total)

	// The heat map: every row, or for a fleet's nodes the extremes.
	var show []string
	if by == statsdb.GPUByNode && len(groups) > maxHeatNodes {
		show = extremes(groups, q.Metric, maxHeatNodes)
	}
	hf := f
	if by == statsdb.GPUByCard {
		hf = cf
	}
	heat := map[string][]rollup.GPU{}
	for i, part := range parts {
		cells, err := s.Stats.GPUHeat(ctx, part, hf, by, show)
		if err != nil {
			return out, err
		}
		for _, c := range cells {
			key := c.Bucket
			if keys[i] >= 0 {
				key = keys[i]
			}
			at := sp.position(key)
			if at < 0 {
				continue
			}
			row := heat[c.Group]
			if row == nil {
				row = make([]rollup.GPU, len(sp.buckets))
				heat[c.Group] = row
			}
			row[at].Add(c.GPU)
		}
	}
	out.HeatTotal = len(heat)
	if groups != nil {
		out.HeatTotal = len(groups)
	}
	if by != statsdb.GPUByModel {
		nodes := make([]string, 0, len(heat))
		for group := range heat {
			nodes = append(nodes, heatNode(group))
		}
		if err := s.nameNodes(sc, nodes); err != nil {
			return out, err
		}
	}
	for group, cells := range heat {
		row := dto.GPUHeatRow{Key: group, Label: sc.heatLabel(by, group), Values: make([]*float64, len(cells))}
		if by != statsdb.GPUByModel {
			row.NodeDeleted = sc.deleted[heatNode(group)]
		}
		for i, c := range cells {
			row.Values[i] = metricValue(c, q.Metric)
		}
		out.Heat = append(out.Heat, row)
	}
	sort.Slice(out.Heat, func(i, j int) bool { return out.Heat[i].Label < out.Heat[j].Label })
	earliest, err := s.earliest(ctx, sp, statsdb.KindGPU)
	if err != nil {
		return out, err
	}
	out.Range = sp.rangeDTO(s.Retention().TimeZone, earliest, s.LiveSinceMS)
	return out, nil
}

// gpuCards sums each card over the parts of a span.
func (s *Service) gpuCards(ctx context.Context, parts []statsdb.Range, f statsdb.GPUFilter) (map[cardKey]*statsdb.GPUCard, error) {
	cards := map[cardKey]*statsdb.GPUCard{}
	for _, part := range parts {
		rows, err := s.Stats.GPUCards(ctx, part, f)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			k := cardKey{r.NodeUUID, r.GPUIndex, r.GPUUUID}
			cur := cards[k]
			if cur == nil {
				c := r
				cards[k] = &c
				continue
			}
			cur.Add(r.GPU)
			if r.GPUModel != "" {
				cur.GPUModel = r.GPUModel
			}
		}
	}
	return cards, nil
}

// gpuGroups sums each model or node over the parts of a span.
func (s *Service) gpuGroups(ctx context.Context, parts []statsdb.Range, f statsdb.GPUFilter, by statsdb.GPUGroupBy) (map[string]rollup.GPU, error) {
	out := map[string]rollup.GPU{}
	for _, part := range parts {
		rows, err := s.Stats.GPUGroups(ctx, part, f, by)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			cur := out[r.Group]
			cur.Add(r.GPU)
			out[r.Group] = cur
		}
	}
	return out, nil
}

// extremes are the n/2 groups with the highest metric and the n/2 with the
// lowest (groups without the metric count as lowest).
func extremes(groups map[string]rollup.GPU, metric string, n int) []string {
	type ranked struct {
		key   string
		value float64
	}
	list := make([]ranked, 0, len(groups))
	for k, g := range groups {
		v := -1.0
		if m := metricValue(g, metric); m != nil {
			v = *m
		}
		list = append(list, ranked{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].value != list[j].value {
			return list[i].value > list[j].value
		}
		return list[i].key < list[j].key
	})
	out := make([]string, 0, n)
	for _, r := range list[:n/2] {
		out = append(out, r.key)
	}
	for _, r := range list[len(list)-n/2:] {
		out = append(out, r.key)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// heatNode is the node of a node's or a card's heat map row.
func heatNode(group string) string {
	node, _, _ := strings.Cut(group, "/")
	return node
}

// cardLabel names a card: its node and index.
func cardLabel(group string, names map[string]string) string {
	parts := strings.SplitN(group, "/", 3)
	if len(parts) != 3 {
		return group
	}
	name := names[parts[0]]
	if name == "" {
		name = parts[0]
	}
	return name + " #" + parts[1]
}
