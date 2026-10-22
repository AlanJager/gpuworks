package api

import "time"

// linkRates are a node's latest per-NIC and per-RDMA-port rates, keyed by
// metric then by the iface or hca label.
type linkRates map[string]map[string]float64

var linkRateMetrics = []struct{ metric, label string }{
	{"gpuworks_node_nic_rx_bytes_per_sec", "iface"},
	{"gpuworks_node_nic_tx_bytes_per_sec", "iface"},
	{"gpuworks_node_rdma_rx_bytes_per_sec", "hca"},
	{"gpuworks_node_rdma_tx_bytes_per_sec", "hca"},
}

// latestLinkRates reads the rates reported in the last two minutes. A
// metrics store that is missing or failing only leaves the rates out.
func (a *apiHandlers) latestLinkRates(nodeUUID string) linkRates {
	out := linkRates{}
	if a.d.MetricsLatest == nil {
		return out
	}
	since := time.Now().UTC().Add(-2 * time.Minute).UnixMilli()
	for _, m := range linkRateMetrics {
		points, err := a.d.MetricsLatest(m.metric, map[string]string{"node": nodeUUID}, since)
		if err != nil {
			continue
		}
		values := map[string]float64{}
		for _, p := range points {
			values[p.Labels[m.label]] = p.Value
		}
		out[m.metric] = values
	}
	return out
}

func (r linkRates) pair(rxMetric, txMetric, key string) (*float64, *float64) {
	get := func(metric string) *float64 {
		if v, ok := r[metric][key]; ok {
			return &v
		}
		return nil
	}
	return get(rxMetric), get(txMetric)
}

func (r linkRates) nic(iface string) (*float64, *float64) {
	return r.pair("gpuworks_node_nic_rx_bytes_per_sec", "gpuworks_node_nic_tx_bytes_per_sec", iface)
}

func (r linkRates) rdma(hca string) (*float64, *float64) {
	return r.pair("gpuworks_node_rdma_rx_bytes_per_sec", "gpuworks_node_rdma_tx_bytes_per_sec", hca)
}
