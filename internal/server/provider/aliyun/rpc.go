package aliyun

import (
	"context"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// vpcProduct is the VPC OpenAPI (VPCs and vSwitches live there, not in ECS).
var vpcProduct = product{name: "vpc", version: "2016-04-28", host: func(region string) string {
	if region == "" {
		return "vpc.aliyuncs.com"
	}
	return "vpc." + region + ".aliyuncs.com"
}}

// RPC is the driver's signed OpenAPI client, exported for test tooling that
// has to create and release instances. The product driver itself deliberately
// never does (see Capabilities), so create/release stay out of the Driver and
// callers get only the raw signed transport, with the same retry, endpoint
// fallback and STS handling the driver uses.
type RPC struct{ c *client }

// NewRPC returns an RPC client with production defaults. A non-empty
// Credentials.Endpoint redirects every call to that URL (tests, gateways).
func NewRPC() *RPC { return &RPC{c: newClient()} }

// Call invokes action of the named product ("ecs" or "vpc") in region and
// decodes the JSON response into out (nil discards it). Parameters travel in
// the signed query string, so values must be short; RunInstances is fine.
func (r *RPC) Call(ctx context.Context, creds provider.Credentials, productName, region, action string, params map[string]string, out any) error {
	var p product
	switch productName {
	case "ecs":
		p = ecsProduct
	case "vpc":
		p = vpcProduct
	default:
		return fmt.Errorf("aliyun: unsupported product %q", productName)
	}
	return r.c.call(ctx, creds, p, region, action, params, out)
}

// ErrorCode returns the Alibaba Cloud error code (for example
// "OperationDenied.NoStock") carried by err, or "" for transport failures and
// errors from elsewhere.
func ErrorCode(err error) string {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}
