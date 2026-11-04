package templateexport

import (
	"context"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/healthcheck"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// CheckTimeout bounds each operation request an export sends.
const CheckTimeout = 60 * time.Second

// ProbeReply is what one request through the gateway answered.
type ProbeReply struct {
	NoReplica  bool
	HTTPStatus int
	Message    string
}

// Prober sends one request to a deployment through the gateway, the way the
// health check does (proxy.Probe): unaccounted, to whichever replica the
// gateway picks.
type Prober func(ctx context.Context, deploymentID uint, path string, payload map[string]any) ProbeReply

// Checks sends the smallest request each cheap operation of contract takes -
// chat, text completion and embeddings, with the health check's own bodies -
// and lists every other operation as not tested: a video, image or speech
// request is slow or costly, and the export says so instead of skipping the
// template.
func Checks(ctx context.Context, probe Prober, deploymentID uint, contract inferenceapi.Contract) []Check {
	checks := make([]Check, 0, len(contract.Operations))
	for _, op := range contract.Operations {
		check := Check{Operation: op.Type, Endpoint: op.Endpoint}
		kind, cheap := checkKind(op.Type)
		if !cheap || probe == nil || op.Endpoint == "" {
			checks = append(checks, check)
			continue
		}
		check.Tested = true
		cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
		reply := probe(cctx, deploymentID, op.Endpoint, healthcheck.Payload(kind))
		cancel()
		check.HTTPStatus, check.Message = reply.HTTPStatus, reply.Message
		switch {
		case reply.NoReplica:
			check.Message = "no running replica"
		case reply.HTTPStatus >= http.StatusOK && reply.HTTPStatus < http.StatusMultipleChoices:
			check.Passed = true
		}
		checks = append(checks, check)
	}
	return checks
}

func checkKind(operation string) (healthcheck.Kind, bool) {
	switch operation {
	case inferenceapi.OperationChat:
		return healthcheck.KindChat, true
	case inferenceapi.OperationCompletions:
		return healthcheck.KindCompletions, true
	case inferenceapi.OperationEmbeddings:
		return healthcheck.KindEmbeddings, true
	}
	return "", false
}
