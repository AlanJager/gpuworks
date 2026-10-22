package deploy

import (
	"strings"

	"gpuworks.ai/gpuworks/pkg/localization"
)

const (
	LeastRequest   = "least_request"
	PrefixAffinity = "prefix_affinity"
	Failover       = "failover"
)

var routingPolicyValues = [...]string{LeastRequest, PrefixAffinity, Failover}

func NormalizeRoutingPolicy(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return LeastRequest
	}
	return value
}

func ValidRoutingPolicy(value string) bool {
	value = NormalizeRoutingPolicy(value)
	for _, item := range routingPolicyValues {
		if item == value {
			return true
		}
	}
	return false
}

func RoutingPolicyValues() []string {
	return append([]string(nil), routingPolicyValues[:]...)
}

type RoutingPolicyOption struct {
	Value       string
	Label       localization.Text
	Description localization.Text
}

func RoutingPolicyOptions() []RoutingPolicyOption {
	return []RoutingPolicyOption{
		{Value: LeastRequest,
			Label:       localization.Text{localization.English: "Load balance", localization.Chinese: "负载均衡"},
			Description: localization.Text{localization.English: "Send each request to the replica with the lightest current load.", localization.Chinese: "把请求发给当前负载最轻的副本。"}},
		{Value: PrefixAffinity,
			Label:       localization.Text{localization.English: "Prefix affinity", localization.Chinese: "前缀亲和"},
			Description: localization.Text{localization.English: "Keep the same prompt prefix on one replica when possible, with overflow when it is hot.", localization.Chinese: "相同提示词前缀尽量固定到同一个副本，热点过载时溢出到其他副本。"}},
		{Value: Failover,
			Label:       localization.Text{localization.English: "Failover", localization.Chinese: "主备"},
			Description: localization.Text{localization.English: "Prefer a stable primary replica and switch when it cannot accept the request.", localization.Chinese: "优先使用稳定的主副本，主副本无法接收请求时切换。"}},
	}
}
