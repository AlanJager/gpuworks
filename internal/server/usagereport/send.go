package usagereport

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Endpoint kinds. The report goes to the endpoint that answered a probe
// fastest (probe.go); the next is tried only when that one fails, so an
// installation normally sends its report once a day.
const (
	// EndpointSLS is an Aliyun Log Service logstore open to WebTracking
	// (PutWebtracking): reachable from the mainland without a filed domain,
	// and its own query engine is the dashboard.
	EndpointSLS = "sls"
	// EndpointCollector is the publisher's Cloudflare Worker: it takes the
	// gzip report as is and keeps it for import into the same logstore.
	EndpointCollector = "collector"
)

// Endpoint is one place a report can be delivered.
type Endpoint struct {
	Kind string
	// URL is the full request URL: for SLS
	// https://<project>.<region>.log.aliyuncs.com/logstores/<logstore>/track.
	URL string
}

// DefaultEndpoints are where installations report, in order. They are
// constants of the binary: an installation is never told to report anywhere
// else.
var DefaultEndpoints = []Endpoint{
	{Kind: EndpointSLS, URL: "https://gpuworks-usage.cn-shanghai.log.aliyuncs.com/logstores/reports/track"},
	{Kind: EndpointCollector, URL: "https://usage.gpuworks.ai/v1/report"},
}

// sendTimeout bounds one delivery attempt; the report is small, so a slow
// endpoint is a failed one.
const sendTimeout = 10 * time.Second

// Sender delivers reports.
type Sender struct {
	// HTTP is the client requests go through (the server's outbound client);
	// nil uses http.DefaultClient.
	HTTP      *http.Client
	Endpoints []Endpoint
}

// Attempt is how one endpoint answered.
type Attempt struct {
	Endpoint string
	Err      error
}

// Send probes the endpoints, records what it measured in the report's
// Delivery, and delivers the report to the fastest endpoint, falling back to
// the others in order of speed. It returns the kind of the endpoint that took
// it, with every attempt made. It never retries an endpoint.
func (s *Sender) Send(ctx context.Context, r *Report) (string, []Attempt, error) {
	endpoints := s.Endpoints
	if endpoints == nil {
		endpoints = DefaultEndpoints
	}
	ordered, ms := s.probe(ctx, endpoints)
	r.Delivery = Delivery{ProbeMS: Counts{}}
	for i, ep := range endpoints {
		r.Delivery.ProbeMS = append(r.Delivery.ProbeMS, Count{Name: ep.Kind, N: ms[i]})
	}
	if len(ordered) > 0 {
		r.Delivery.Endpoint = ordered[0].Kind
	}
	body, err := Encode(r)
	if err != nil {
		return "", nil, err
	}
	var attempts []Attempt
	for _, ep := range ordered {
		err := s.deliver(ctx, ep, r, body)
		attempts = append(attempts, Attempt{Endpoint: ep.Kind, Err: err})
		if err == nil {
			return ep.Kind, attempts, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	if len(attempts) == 0 {
		return "", nil, errors.New("no usage report endpoint")
	}
	return "", attempts, fmt.Errorf("usage report not delivered: %w", attempts[len(attempts)-1].Err)
}

func (s *Sender) deliver(ctx context.Context, ep Endpoint, r *Report, report []byte) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	var req *http.Request
	var err error
	switch ep.Kind {
	case EndpointSLS:
		req, err = slsRequest(ctx, ep, r, report)
	case EndpointCollector:
		req, err = collectorRequest(ctx, ep, report)
	default:
		return fmt.Errorf("unknown endpoint kind %q", ep.Kind)
	}
	if err != nil {
		return err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	return &EndpointError{Status: resp.StatusCode, Code: errorCode(answer)}
}

func (s *Sender) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

// EndpointError is an endpoint's refusal.
type EndpointError struct {
	Status int
	// Code is the SLS errorCode (e.g. LogStoreNotExist) when the body has one.
	Code string
}

func (e *EndpointError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("HTTP %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

func errorCode(body []byte) string {
	var answer struct {
		ErrorCode string `json:"errorCode"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return ""
	}
	return answer.ErrorCode
}

// slsLogGroup is the PutWebtracking body. SLS log values are strings, so the
// report rides whole as one JSON string next to the keys the collector
// filters on. The service refuses any other top-level key (PostBodyInvalid
// "only support keys{__topic__, __source__, __logs__, __tags__}"), although
// its API reference lists project and logstoreName in the body too.
type slsLogGroup struct {
	Topic    string              `json:"__topic__"`
	Source   string              `json:"__source__"`
	Logs     []map[string]string `json:"__logs__"`
}

func slsRequest(ctx context.Context, ep Endpoint, r *Report, report []byte) (*http.Request, error) {
	group := slsLogGroup{
		Topic: "usage", Source: "gpuworks-server",
		Logs: []map[string]string{{
			"schema":     strconv.Itoa(r.Schema),
			"install_id": r.InstallID,
			"day":        r.Day,
			"version":    r.Server.Version,
			"report":     string(report),
		}},
	}
	raw, err := json.Marshal(group)
	if err != nil {
		return nil, err
	}
	var packed bytes.Buffer
	zw := zlib.NewWriter(&packed)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, &packed)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-log-apiversion", "0.6.0")
	req.Header.Set("x-log-bodyrawsize", strconv.Itoa(len(raw)))
	req.Header.Set("x-log-compresstype", "deflate")
	return req, nil
}

func collectorRequest(ctx context.Context, ep Endpoint, report []byte) (*http.Request, error) {
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	if _, err := zw.Write(report); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, &packed)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	return req, nil
}
