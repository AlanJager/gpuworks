package aliyun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestRPCRoutesProductsAndSurfacesErrorCodes(t *testing.T) {
	var gotVersion, gotAction, gotRegion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion, gotAction, gotRegion = r.Header.Get("x-acs-version"), r.Header.Get("x-acs-action"), r.URL.Query().Get("RegionId")
		w.Header().Set("Content-Type", "application/json")
		if gotAction == "RunInstances" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"Code": "OperationDenied.NoStock", "Message": "sold out", "RequestId": "REQ"})
			return
		}
		fmt.Fprint(w, `{"VpcId":"vpc-1"}`)
	}))
	defer srv.Close()
	creds := provider.Credentials{PublicKey: "id", PrivateKey: "secret", Endpoint: srv.URL}
	rpc := NewRPC()

	var out struct {
		VpcID string `json:"VpcId"`
	}
	if err := rpc.Call(context.Background(), creds, "vpc", "cn-hangzhou", "CreateVpc", map[string]string{"CidrBlock": "172.30.0.0/16"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.VpcID != "vpc-1" || gotVersion != "2016-04-28" || gotAction != "CreateVpc" || gotRegion != "cn-hangzhou" {
		t.Fatalf("vpc call: out=%+v version=%q action=%q region=%q", out, gotVersion, gotAction, gotRegion)
	}
	err := rpc.Call(context.Background(), creds, "ecs", "cn-hangzhou", "RunInstances", nil, nil)
	if got := ErrorCode(err); got != "OperationDenied.NoStock" || gotVersion != "2014-05-26" {
		t.Fatalf("ecs error code = %q version=%q err=%v", got, gotVersion, err)
	}
	if err := rpc.Call(context.Background(), creds, "rds", "", "Describe", nil, nil); err == nil {
		t.Fatal("an unknown product must be refused")
	}
	if ErrorCode(errors.New("dial tcp: refused")) != "" || ErrorCode(nil) != "" {
		t.Fatal("transport errors carry no API code")
	}
}
