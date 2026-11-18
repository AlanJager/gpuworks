package aliyun

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
)

// ehpcClusterDetailJSON is a GetCluster (2024-07-30) answer for a Slurm
// cluster: the manager node is an ECS instance, named under
// Manager.ManagerNode.
func ehpcClusterDetailJSON(managerID string) map[string]any {
	manager := map[string]any{
		"Scheduler": map[string]any{"Type": "slurm", "Version": "22.05.8"},
		"DNS":       map[string]any{"Type": "NIS"},
	}
	if managerID != "" {
		manager["ManagerNode"] = map[string]any{"InstanceId": managerID, "InstanceType": "ecs.c7.large", "ImageId": "centos_7_9_x64_20G_alibase.vhd", "SystemDisk": map[string]any{"Size": 40}}
	}
	return map[string]any{
		"RequestId": "req-getcluster", "ClusterId": "ehpc-sz-iqinzafcg4", "ClusterName": "gpuworks-slurm-test", "ClusterCategory": "Standard",
		"ClusterMode": "Integrated", "ClusterStatus": "running", "EhpcVersion": "2.0.0", "Manager": manager,
	}
}

// managerFake serves the E-HPC and Cloud Assistant calls of one run on the
// manager node. A nil handler for a call answers as the real API does when it
// succeeds; override replaces the answer of one action.
func managerFake(t *testing.T, managerID string, online bool, status string, exitCode int, output string, override map[string]func(fakeCall) (int, any)) (*Driver, *fakeAliyun) {
	polls := 0
	return newFake(t, func(c fakeCall) (int, any) {
		if handler := override[c.action]; handler != nil {
			return handler(c)
		}
		switch c.action {
		case "GetCluster":
			return 200, ehpcClusterDetailJSON(managerID)
		case "DescribeCloudAssistantStatus":
			return 200, map[string]any{"InstanceCloudAssistantStatusSet": map[string]any{"InstanceCloudAssistantStatus": []any{
				map[string]any{"InstanceId": c.query["InstanceId.1"], "CloudAssistantStatus": map[bool]string{true: "true", false: "false"}[online]},
			}}}
		case "RunCommand":
			return 200, map[string]string{"CommandId": "c-1", "InvokeId": "t-1", "RequestId": "req-run"}
		case "DescribeInvocationResults":
			polls++
			shown := status
			if polls == 1 {
				shown = "Running"
			}
			return 200, map[string]any{"Invocation": map[string]any{"InvocationResults": map[string]any{"InvocationResult": []any{map[string]any{
				"InvokeId": "t-1", "InstanceId": c.query["InstanceId"], "InvocationStatus": shown, "ExitCode": exitCode,
				"Output": base64.StdEncoding.EncodeToString([]byte(output)), "ErrorInfo": "",
			}}}}}
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound", "Message": c.action}
	})
}

func (f *fakeAliyun) call(action string) fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.action == action {
			return c
		}
	}
	return fakeCall{}
}

const managerScript = "echo setup"

func runOnManager(d *Driver) (provider.SlurmManagerRun, error) {
	return d.RunOnSlurmManager(context.Background(), userCreds, "cn-shenzhen", "ehpc-sz-iqinzafcg4", managerScript, 5*time.Minute)
}

func TestRunOnSlurmManagerRunsTheScriptOnTheClustersManager(t *testing.T) {
	d, fake := managerFake(t, "i-mgr", true, "Success", 0, "GWKS_SLURM step=done status=ok\n", nil)
	run, err := runOnManager(d)
	if err != nil {
		t.Fatal(err)
	}
	if run != (provider.SlurmManagerRun{ManagerInstance: "i-mgr", Output: "GWKS_SLURM step=done status=ok\n"}) {
		t.Fatalf("run = %+v", run)
	}
	if got := strings.Join(fake.actions(), ","); got != "GetCluster,DescribeCloudAssistantStatus,RunCommand,DescribeInvocationResults,DescribeInvocationResults" {
		t.Fatalf("calls = %s", got)
	}
	if c := fake.call("GetCluster"); c.host != "ehpc.cn-shenzhen.aliyuncs.com" || c.query["ClusterId"] != "ehpc-sz-iqinzafcg4" || c.query["RegionId"] != "cn-shenzhen" {
		t.Fatalf("GetCluster = %+v", c)
	}
	if c := fake.call("DescribeCloudAssistantStatus"); c.query["InstanceId.1"] != "i-mgr" || c.query["RegionId"] != "cn-shenzhen" {
		t.Fatalf("DescribeCloudAssistantStatus = %+v", c)
	}
	run1 := fake.call("RunCommand")
	if run1.query["InstanceId.1"] != "i-mgr" || run1.query["Type"] != "RunShellScript" || run1.query["Timeout"] != "300" || run1.query["RegionId"] != "cn-shenzhen" ||
		run1.query["Name"] != "gpuworks-slurm-setup" || run1.query["CommandContent"] != managerScript {
		t.Fatalf("RunCommand = %+v", run1.query)
	}
	if poll := fake.call("DescribeInvocationResults"); poll.query["InstanceId"] != "i-mgr" || poll.query["InvokeId"] != "t-1" {
		t.Fatalf("DescribeInvocationResults = %+v", poll.query)
	}
}

func TestRunOnSlurmManagerNamesTheMissingPermission(t *testing.T) {
	forbidden := func(fakeCall) (int, any) {
		return 403, map[string]string{"Code": "NotAuthorized", "Message": "You are not authorized by RAM for this request.", "RequestId": "req-ram"}
	}
	for _, tc := range []struct {
		action, permission, ran string
	}{
		{"GetCluster", "ehpc:GetCluster", "GetCluster"},
		{"DescribeCloudAssistantStatus", "ecs:DescribeCloudAssistantStatus", "GetCluster,DescribeCloudAssistantStatus"},
		{"RunCommand", "ecs:RunCommand", "GetCluster,DescribeCloudAssistantStatus,RunCommand"},
		{"DescribeInvocationResults", "ecs:DescribeInvocationResults", "GetCluster,DescribeCloudAssistantStatus,RunCommand,DescribeInvocationResults"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			d, fake := managerFake(t, "i-mgr", true, "Success", 0, "", map[string]func(fakeCall) (int, any){tc.action: forbidden})
			_, err := runOnManager(d)
			var perm *provider.SlurmApplyPermissionError
			if !errors.As(err, &perm) || perm.Permission != tc.permission {
				t.Fatalf("err = %v; want permission %s", err, tc.permission)
			}
			if got := strings.Join(fake.actions(), ","); got != tc.ran {
				t.Fatalf("calls = %s; want %s", got, tc.ran)
			}
		})
	}
	// Forbidden.RAM, the code ECS answers with, is the same refusal.
	d, _ := managerFake(t, "i-mgr", true, "Success", 0, "", map[string]func(fakeCall) (int, any){"RunCommand": func(fakeCall) (int, any) {
		return 403, map[string]string{"Code": "Forbidden.RAM", "Message": "User not authorized to operate on the specified resource.", "RequestId": "req-forbidden"}
	}})
	_, err := runOnManager(d)
	var perm *provider.SlurmApplyPermissionError
	if !errors.As(err, &perm) || perm.Permission != "ecs:RunCommand" {
		t.Fatalf("Forbidden.RAM err = %v", err)
	}
}

func TestRunOnSlurmManagerWithoutAManagerRunsNothing(t *testing.T) {
	d, fake := managerFake(t, "", true, "Success", 0, "", nil)
	if _, err := runOnManager(d); !errors.Is(err, provider.ErrSlurmApplyManagerUnknown) {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(fake.actions(), ","); got != "GetCluster" {
		t.Fatalf("calls = %s", got)
	}
}

func TestRunOnSlurmManagerReportsWhatTheCloudRefusesOrCannotReach(t *testing.T) {
	var failed *provider.SlurmApplyFailedError

	// E-HPC does not know the cluster.
	d, _ := managerFake(t, "i-mgr", true, "Success", 0, "", map[string]func(fakeCall) (int, any){"GetCluster": func(fakeCall) (int, any) {
		return 404, map[string]string{"Code": "InvalidClusterId.NotFound", "Message": "The specified cluster does not exist.", "RequestId": "req-nf"}
	}})
	_, err := runOnManager(d)
	if !errors.As(err, &failed) || failed.Step != messages.SlurmApplyStepCluster || !strings.Contains(failed.Output, "InvalidClusterId.NotFound") {
		t.Fatalf("unknown cluster err = %v", err)
	}

	// Cloud Assistant on the manager is offline: nothing is run.
	d, fake := managerFake(t, "i-mgr", false, "Success", 0, "", nil)
	_, err = runOnManager(d)
	if !errors.As(err, &failed) || failed.Step != messages.SlurmApplyStepAssistant || !strings.Contains(failed.Output, "not online") {
		t.Fatalf("offline err = %v", err)
	}
	if got := strings.Join(fake.actions(), ","); got != "GetCluster,DescribeCloudAssistantStatus" {
		t.Fatalf("calls = %s", got)
	}

	// RunCommand itself refused for a reason that is not a permission.
	d, _ = managerFake(t, "i-mgr", true, "Success", 0, "", map[string]func(fakeCall) (int, any){"RunCommand": func(fakeCall) (int, any) {
		return 400, map[string]string{"Code": "InvalidParameter.CommandContent", "Message": "too long", "RequestId": "req-long"}
	}})
	_, err = runOnManager(d)
	if !errors.As(err, &failed) || failed.Step != messages.SlurmApplyStepRun || !strings.Contains(failed.Output, "InvalidParameter.CommandContent") {
		t.Fatalf("refused command err = %v", err)
	}
}

// A script that ran and failed is not an error of the driver: the output and
// how the cloud ended it come back for the caller to read.
func TestRunOnSlurmManagerReturnsTheOutputOfAFailedScript(t *testing.T) {
	d, _ := managerFake(t, "i-mgr", true, "Failed", 127, "sh: 1: scontrol: not found\n", nil)
	run, err := runOnManager(d)
	if err != nil {
		t.Fatal(err)
	}
	if run.ManagerInstance != "i-mgr" || run.Output != "sh: 1: scontrol: not found\n" || !strings.Contains(run.Ended, "ended Failed with exit code 127") {
		t.Fatalf("run = %+v", run)
	}
}
