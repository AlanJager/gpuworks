package analytics

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

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

// GPUQuery is a GPU utilization query: the range, what the heat map's rows
// are, what its colour is, and filters.
type GPUQuery struct {
	Query
	GroupBy string // model | node | gpu
	Metric  string // util | power | temp | mem
	statsdb.GPUFilter
}

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

type cardKey struct {
	node  string
	index int
	uuid  string
}

func (k cardKey) group() string { return k.node + "/" + strconv.Itoa(k.index) + "/" + k.uuid }

// GPUs answers the GPU utilization over a range.
func (s *Service) GPUs(ctx context.Context, q GPUQuery) (dto.GPUAnalytics, error) {
	if q.GroupBy == "" {
		q.GroupBy = string(statsdb.GPUByModel)
	}
	if q.Metric == "" {
		q.Metric = MetricUtil
	}
	switch statsdb.GPUGroupBy(q.GroupBy) {
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
	parts, keys, err := s.statsParts(ctx, sp)
	if err != nil {
		return dto.GPUAnalytics{}, err
	}
	cards := map[cardKey]*statsdb.GPUCard{}
	for _, part := range parts {
		rows, err := s.Stats.GPUCards(ctx, part, q.GPUFilter)
		if err != nil {
			return dto.GPUAnalytics{}, err
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
	if q.GroupBy == string(statsdb.GPUByCard) && len(cards) > maxHeatCardsByGPU {
		return dto.GPUAnalytics{}, queryError(messages.ErrorAnalyticsTooManyCards, "group_by",
			fmt.Sprintf("%d cards; filter by node or model to see at most %d by card", len(cards), maxHeatCardsByGPU))
	}
	heat := map[string][]rollup.GPU{}
	for i, part := range parts {
		cells, err := s.Stats.GPUHeat(ctx, part, q.GPUFilter, statsdb.GPUGroupBy(q.GroupBy))
		if err != nil {
			return dto.GPUAnalytics{}, err
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

	nodes := map[string]bool{}
	for k := range cards {
		nodes[k.node] = true
	}
	nodeList := make([]string, 0, len(nodes))
	for n := range nodes {
		nodeList = append(nodeList, n)
	}
	nodeNames, err := s.Store.NodeNamesByUUID(nodeList)
	if err != nil {
		return dto.GPUAnalytics{}, err
	}
	present, holders, err := s.cardsNow()
	if err != nil {
		return dto.GPUAnalytics{}, err
	}
	out := dto.GPUAnalytics{Metric: q.Metric, GroupBy: q.GroupBy, Cards: []dto.GPUCardRow{}, Heat: []dto.GPUHeatRow{}}
	var total rollup.GPU
	for k, c := range cards {
		total.Add(c.GPU)
		row := dto.GPUCardRow{NodeUUID: k.node, NodeName: nodeNames[k.node], GPUIndex: k.index, GPUUUID: k.uuid,
			GPUModel: c.GPUModel, Present: present[k], Deployments: holders[k], GPUFigures: gpuFigures(c.GPU)}
		if row.Deployments == nil {
			row.Deployments = []string{}
		}
		out.Cards = append(out.Cards, row)
	}
	sort.Slice(out.Cards, func(i, j int) bool {
		a, b := out.Cards[i], out.Cards[j]
		if a.NodeName != b.NodeName {
			return a.NodeName < b.NodeName
		}
		return a.GPUIndex < b.GPUIndex
	})
	out.Totals = gpuFigures(total)
	for group, cells := range heat {
		row := dto.GPUHeatRow{Key: group, Label: s.heatLabel(q.GroupBy, group, nodeNames), Values: make([]*float64, len(cells))}
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

func (s *Service) heatLabel(groupBy, group string, nodeNames map[string]string) string {
	switch statsdb.GPUGroupBy(groupBy) {
	case statsdb.GPUByNode:
		if n := nodeNames[group]; n != "" {
			return n
		}
	case statsdb.GPUByCard:
		parts := strings.SplitN(group, "/", 3)
		if len(parts) == 3 {
			name := nodeNames[parts[0]]
			if name == "" {
				name = parts[0]
			}
			return name + " #" + parts[1]
		}
	case statsdb.GPUByModel:
		if group == "" {
			return "?"
		}
	}
	return group
}

// cardsNow are the cards of the online nodes now, and the deployments that
// hold each.
func (s *Service) cardsNow() (map[cardKey]bool, map[cardKey][]string, error) {
	snap, err := s.Store.GPUTimeSnapshot()
	if err != nil {
		return nil, nil, err
	}
	present := make(map[cardKey]bool, len(snap.Cards))
	byIdent := map[string]cardKey{}
	for _, c := range snap.Cards {
		node := snap.NodeUUIDs[c.NodeID]
		k := cardKey{node, c.Index, c.UUID}
		present[k] = true
		byIdent[strconv.FormatUint(uint64(c.NodeID), 10)+"\x00"+c.Ident] = k
	}
	deps := map[uint]bool{}
	for _, h := range snap.Holdings {
		deps[h.DeploymentID] = true
	}
	names, err := s.deploymentNames(deps)
	if err != nil {
		return nil, nil, err
	}
	holders := map[cardKey][]string{}
	for _, h := range snap.Holdings {
		name := names[h.DeploymentID].Name
		for _, ident := range h.Idents {
			if k, ok := byIdent[strconv.FormatUint(uint64(h.NodeID), 10)+"\x00"+ident]; ok && !contains(holders[k], name) {
				holders[k] = append(holders[k], name)
			}
		}
	}
	return present, holders, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
