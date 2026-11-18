package aliyun

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func listNodeJSON(id, host, status string, gpus int) map[string]any {
	return map[string]any{"Id": id, "Hostname": host, "Status": status, "InstanceType": "ecs.gn7i-c8g1.2xlarge",
		"TotalResources": map[string]any{"Gpu": gpus, "Cpu": 8, "Memory": 30}, "StateInSched": "active", "QueueName": "gpu"}
}

// nodesFake serves ListNodes for one cluster, Cloud Assistant's status for the
// instances in offline (everything else online) and one command run per
// instance whose output is outputs[instance] and whose outcome is failed[instance].
type nodesFake struct {
	nodes   []map[string]any
	offline map[string]bool
	outputs map[string]string
	failed  map[string]string
	mu      sync.Mutex
	invokes map[string]string // invoke id -> instance
}

func (n *nodesFake) handler(t *testing.T) func(fakeCall) (int, any) {
	n.invokes = map[string]string{}
	return func(c fakeCall) (int, any) {
		switch c.action {
		case "ListNodes":
			if c.query["ClusterId"] != "ehpc-sz-1" || c.query["PageSize"] != "50" {
				t.Errorf("ListNodes = %+v", c.query)
			}
			from := (atoiDefault(c.query["PageNumber"], 1) - 1) * 50
			page := []map[string]any{}
			if from < len(n.nodes) {
				page = n.nodes[from:min(from+50, len(n.nodes))]
			}
			return 200, map[string]any{"TotalCount": len(n.nodes), "Nodes": page, "RequestId": "req-nodes"}
		case "DescribeCloudAssistantStatus":
			var set []any
			for i := 1; c.query[fmt.Sprintf("InstanceId.%d", i)] != ""; i++ {
				id := c.query[fmt.Sprintf("InstanceId.%d", i)]
				set = append(set, map[string]any{"InstanceId": id, "CloudAssistantStatus": map[bool]string{true: "false", false: "true"}[n.offline[id]]})
			}
			return 200, map[string]any{"InstanceCloudAssistantStatusSet": map[string]any{"InstanceCloudAssistantStatus": set}}
		case "RunCommand":
			id := c.query["InstanceId.1"]
			n.mu.Lock()
			defer n.mu.Unlock()
			invoke := "t-" + id
			n.invokes[invoke] = id
			return 200, map[string]string{"CommandId": "c-1", "InvokeId": invoke, "RequestId": "req-run"}
		case "DescribeInvocationResults":
			id := c.query["InstanceId"]
			status, code := "Success", 0
			if f := n.failed[id]; f != "" {
				status, code = f, 1
			}
			return 200, map[string]any{"Invocation": map[string]any{"InvocationResults": map[string]any{"InvocationResult": []any{map[string]any{
				"InvokeId": c.query["InvokeId"], "InstanceId": id, "InvocationStatus": status, "ExitCode": code,
				"Output": base64.StdEncoding.EncodeToString([]byte(n.outputs[id]))}}}}}
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound", "Message": c.action}
	}
}

func atoiDefault(s string, d int) int {
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil {
		return d
	}
	return n
}

func runNodes(d *Driver) ([]provider.SlurmNodeRun, error) {
	return d.RunOnSlurmNodes(context.Background(), userCreds, "cn-shenzhen", "ehpc-sz-1", "echo probe", time.Minute)
}

// Every compute node with a card is asked, through Cloud Assistant, in the
// cluster's region, with the script as given; a CPU node is not. A node that
// is not running, whose Cloud Assistant is offline, or whose command did not
// finish is answered as not asked, with why.
func TestRunOnSlurmNodesAsksEveryGPUNodeAndSaysWhyOthersWereNotAsked(t *testing.T) {
	fake := &nodesFake{
		nodes: []map[string]any{
			listNodeJSON("i-0", "gpu000", "running", 1), listNodeJSON("i-1", "gpu001", "running", 2), listNodeJSON("i-cpu", "cpu000", "running", 0),
			listNodeJSON("i-2", "gpu002", "initing", 1), listNodeJSON("i-3", "gpu003", "running", 1), listNodeJSON("i-4", "gpu004", "running", 1),
		},
		offline: map[string]bool{"i-3": true},
		outputs: map[string]string{"i-0": "GWKS_REACH rc=0\n", "i-1": "GWKS_REACH rc=28\n"},
		failed:  map[string]string{"i-4": "Failed"},
	}
	d, api := newFake(t, fake.handler(t))
	runs, err := runNodes(d)
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.SlurmNodeRun{
		{Name: "gpu000", InstanceID: "i-0", Output: "GWKS_REACH rc=0\n"},
		{Name: "gpu001", InstanceID: "i-1", Output: "GWKS_REACH rc=28\n"},
		{Name: "gpu002", InstanceID: "i-2", NotAsked: provider.SlurmReachNodeNotRunning},
		{Name: "gpu003", InstanceID: "i-3", NotAsked: provider.SlurmReachAssistantOffline},
		{Name: "gpu004", InstanceID: "i-4", NotAsked: provider.SlurmReachCommandFailed},
	}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs\n got %+v\nwant %+v", runs, want)
	}
	if c := api.call("ListNodes"); c.host != "ehpc.cn-shenzhen.aliyuncs.com" || c.query["RegionId"] != "cn-shenzhen" {
		t.Fatalf("ListNodes = %+v", c)
	}
	// Only the running nodes' Cloud Assistant is asked about, and only the online ones are run on.
	if c := api.call("DescribeCloudAssistantStatus"); c.query["InstanceId.1"] != "i-0" || c.query["InstanceId.4"] != "i-4" || c.query["InstanceId.5"] != "" || c.query["RegionId"] != "cn-shenzhen" {
		t.Fatalf("DescribeCloudAssistantStatus = %+v", c.query)
	}
	ran := map[string]bool{}
	api.mu.Lock()
	for _, c := range api.calls {
		if c.action == "RunCommand" {
			ran[c.query["InstanceId.1"]] = true
			if c.query["Type"] != "RunShellScript" || c.query["CommandContent"] != "echo probe" || c.query["Name"] != "gpuworks-slurm-reach" || c.query["Timeout"] != "60" || c.query["RegionId"] != "cn-shenzhen" {
				t.Errorf("RunCommand = %+v", c.query)
			}
		}
	}
	api.mu.Unlock()
	if !reflect.DeepEqual(ran, map[string]bool{"i-0": true, "i-1": true, "i-4": true}) {
		t.Fatalf("commands ran on %v", ran)
	}
}

// A cluster with more nodes than a page is read to its end, and no more than a
// few nodes are run on at once.
func TestRunOnSlurmNodesReadsEveryPageAndBoundsTheParallelism(t *testing.T) {
	var nodes []map[string]any
	for i := 0; i < 60; i++ {
		nodes = append(nodes, listNodeJSON(fmt.Sprintf("i-%02d", i), fmt.Sprintf("gpu%03d", i), "running", 1))
	}
	fake := &nodesFake{nodes: nodes, outputs: map[string]string{}}
	var (
		mu                sync.Mutex
		inFlight, highest int
	)
	handler := fake.handler(t)
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "RunCommand":
			mu.Lock()
			inFlight++
			highest = max(highest, inFlight)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		case "DescribeInvocationResults":
			mu.Lock()
			inFlight--
			mu.Unlock()
		}
		return handler(c)
	})
	runs, err := runNodes(d)
	if err != nil || len(runs) != 60 {
		t.Fatalf("%d runs, err %v", len(runs), err)
	}
	if highest > slurmNodeParallel {
		t.Fatalf("%d commands at once, bound is %d", highest, slurmNodeParallel)
	}
}

// A key that may not list the cluster's nodes, or may not run commands, is the
// missing permission; any other failure to list is the failure.
func TestRunOnSlurmNodesNamesTheMissingPermission(t *testing.T) {
	forbidden := func(fakeCall) (int, any) {
		return 403, map[string]string{"Code": "NotAuthorized", "Message": "You are not authorized by RAM for this request.", "RequestId": "req-ram"}
	}
	for _, action := range []string{"ListNodes", "DescribeCloudAssistantStatus"} {
		fake := &nodesFake{nodes: []map[string]any{listNodeJSON("i-0", "gpu000", "running", 1)}}
		handler := fake.handler(t)
		d, _ := newFake(t, func(c fakeCall) (int, any) {
			if c.action == action {
				return forbidden(c)
			}
			return handler(c)
		})
		_, err := runNodes(d)
		var perm *provider.SlurmApplyPermissionError
		want := map[string]string{"ListNodes": "ehpc:ListNodes", "DescribeCloudAssistantStatus": "ecs:DescribeCloudAssistantStatus"}[action]
		if !errors.As(err, &perm) || perm.Permission != want {
			t.Errorf("%s: err = %v, want permission %s", action, err, want)
		}
	}
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		return 404, map[string]string{"Code": "InvalidClusterId.NotFound", "Message": "The specified cluster does not exist.", "RequestId": "req-nf"}
	})
	if _, err := runNodes(d); err == nil || strings.Contains(err.Error(), "permission") {
		t.Fatalf("err = %v", err)
	}
}

// A cluster whose nodes carry no card, or that has none, is asked about nothing.
func TestRunOnSlurmNodesWithoutGPUNodesRunsNothing(t *testing.T) {
	fake := &nodesFake{nodes: []map[string]any{listNodeJSON("i-cpu", "cpu000", "running", 0)}}
	d, api := newFake(t, fake.handler(t))
	runs, err := runNodes(d)
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs %+v, err %v", runs, err)
	}
	if got := strings.Join(api.actions(), ","); got != "ListNodes" {
		t.Fatalf("calls = %s", got)
	}
}
