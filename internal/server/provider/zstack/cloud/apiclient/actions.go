package apiclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PutAction submits a mutation once and waits for its final asynchronous result.
func (c *APIClient) PutAction(ctx context.Context, path string, payload any, options ...ClientOption) error {
	return c.putAction(ctx, path, payload, "", nil, options...)
}

// PutActionResult decodes the completed action's response, including when the
// initial response is asynchronous. Some APIs report per-resource failures
// inside a successful response; their callers must inspect that result.
func (c *APIClient) PutActionResult(ctx context.Context, path string, payload, out any, options ...ClientOption) error {
	return c.putAction(ctx, path, payload, "", out, options...)
}

// PutActionWithJob correlates a mutation with a job ID persisted by its caller.
// The ID must be fresh: ZStack rejects duplicate X-Job-UUID submissions.
func (c *APIClient) PutActionWithJob(ctx context.Context, path string, payload any, jobID string, options ...ClientOption) error {
	if !validJobID(jobID) {
		return &APIError{Kind: ErrorRequest}
	}
	return c.putAction(ctx, path, payload, jobID, nil, options...)
}

func validJobID(id string) bool {
	_, err := hex.DecodeString(id)
	return len(id) == 32 && err == nil
}

func (c *APIClient) putAction(ctx context.Context, path string, payload any, submittedJob string, out any, options ...ClientOption) error {
	policy, err := c.policy.override(options)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return &APIError{Kind: ErrorRequest}
	}
	ctx, cancel := context.WithTimeout(ctx, policy.operationTimeout)
	defer cancel()
	submittedAt := time.Now()
	reply, err := c.request(ctx, policy, http.MethodPut, path, raw, submittedJob)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && submittedJob != "" {
			copy := *apiErr
			copy.JobID = submittedJob
			return &copy
		}
		return err
	}
	if reply.status != http.StatusAccepted {
		return decodeActionResult(reply, out)
	}
	jobPath, err := actionJobPath(reply.Location)
	if err != nil {
		return jobError(&APIError{Kind: ErrorResponse}, submittedJob)
	}
	jobID, _ := url.PathUnescape(strings.TrimPrefix(jobPath, "/api-jobs/"))
	if submittedJob != "" && jobID != submittedJob {
		return jobError(&APIError{Kind: ErrorResponse}, submittedJob)
	}
	if reply.APITimeout < 0 {
		return jobError(&APIError{Kind: ErrorResponse}, jobID)
	}
	deadline, _ := ctx.Deadline()
	// Compare milliseconds before conversion so a malformed large timeout
	// cannot overflow a Duration or extend the original operation budget.
	if reply.APITimeout > 0 && reply.APITimeout < deadline.Sub(submittedAt).Milliseconds() {
		var jobCancel context.CancelFunc
		ctx, jobCancel = context.WithDeadline(ctx, submittedAt.Add(time.Duration(reply.APITimeout)*time.Millisecond))
		defer jobCancel()
	}
	return c.waitForJob(ctx, policy, jobPath, jobID, out)
}

// WaitForJob resumes observation without submitting another mutation.
func (c *APIClient) WaitForJob(ctx context.Context, jobID string, options ...ClientOption) error {
	if !validJobID(jobID) {
		return jobError(&APIError{Kind: ErrorRequest}, jobID)
	}
	policy, err := c.policy.override(options)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, policy.operationTimeout)
	defer cancel()
	return c.waitForJob(ctx, policy, "/api-jobs/"+jobID, jobID, nil)
}

func (c *APIClient) waitForJob(ctx context.Context, policy clientPolicy, jobPath, jobID string, out any) error {
	for {
		reply, err := c.request(ctx, policy, http.MethodGet, jobPath, nil, "")
		if err != nil {
			return jobError(err, jobID)
		}
		if reply.status == http.StatusOK {
			if err := decodeActionResult(reply, out); err != nil {
				return jobError(err, jobID)
			}
			return nil
		}
		if err := waitForCloud(ctx, policy.pollInterval); err != nil {
			return jobError(transportError(err), jobID)
		}
	}
}

func decodeActionResult(reply cloudReply, out any) error {
	if out != nil {
		if err := json.Unmarshal(reply.body, out); err != nil {
			return &APIError{Kind: ErrorResponse, StatusCode: reply.status, OutcomeUnknown: true}
		}
	}
	return nil
}

// Cloud may advertise an internal management address. Use only its job identity
// and keep every authenticated request on the configured endpoint.
func actionJobPath(location string) (string, error) {
	u, err := url.Parse(location)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("zstack: invalid async job location")
	}
	const prefix = "/v1/api-jobs/"
	index := strings.LastIndex(u.Path, prefix)
	if index < 0 {
		return "", fmt.Errorf("zstack: missing async job location")
	}
	id := strings.TrimPrefix(u.Path[index:], prefix)
	if id == "" || strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return "", fmt.Errorf("zstack: invalid async job identity")
	}
	return "/api-jobs/" + url.PathEscape(id), nil
}
