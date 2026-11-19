package aliyun

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// ehpcNodeJSON is an E-HPC compute node as DescribeInstances lists it: its
// name "<ClusterId>_<hostname>" and the tags E-HPC puts on it (taken from a
// real cluster, 2026-11-15).
func ehpcNodeJSON(id, region, cluster, host string) map[string]any {
	node := instanceJSON(id, region, "Running", "", "172.30.5.44", 1, "NVIDIA T4")
	node["InstanceName"] = cluster + "_" + host
	node["Tags"] = map[string]any{"Tag": []map[string]string{
		{"TagKey": "acs:ecs:payType", "TagValue": "spot"},
		{"TagKey": cluster + "_Queue", "TagValue": "gpu"},
		{"TagKey": "ehpc.aliyun.com", "TagValue": cluster},
	}}
	return node
}

// ehpcClustersBody is a ListClusters (2024-07-30) answer, trimmed from a
// real one.
func ehpcClustersBody(clusters ...map[string]any) map[string]any {
	return map[string]any{"TotalCount": len(clusters), "RequestId": "req-ehpc", "PageSize": 50, "PageNumber": 1, "Clusters": clusters,
		"EhpcVersionStatistics": map[string]any{"V1Count": 0, "V2Count": len(clusters)}}
}

func ehpcClusterJSON(id, name, scheduler, version string) map[string]any {
	return map[string]any{
		"ClusterId": id, "ClusterName": name, "ClusterCategory": "Standard", "ClusterMode": "Integrated", "ClusterStatus": "running",
		"EhpcVersion": "2.0.0", "Manager": map[string]any{"Scheduler": map[string]any{"Type": scheduler, "Version": version}, "DNS": map[string]any{"Type": "NIS"}},
		"Nodes": map[string]any{"RunningCounts": 1, "AbnormalCounts": 0, "CreatingCounts": 0},
	}
}

func ehpcFake(t *testing.T, listClusters func(fakeCall) (int, any)) (*Driver, *fakeAliyun) {
	return newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeRegions":
			return 200, regionsBody("cn-shenzhen", "cn-beijing")
		case "DescribeInstances":
			switch c.query["RegionId"] {
			case "cn-shenzhen":
				return 200, map[string]any{"Instances": map[string]any{"Instance": []any{
					ehpcNodeJSON("i-gpu000", "cn-shenzhen", "ehpc-sz-iqinzafcg4", "gpu000"),
					ehpcNodeJSON("i-pbs000", "cn-shenzhen", "ehpc-sz-pbs", "comp000"),
					instanceJSON("i-plain", "cn-shenzhen", "Running", "", "172.30.5.9", 1, "NVIDIA T4"),
				}}, "NextToken": ""}
			case "cn-beijing":
				return 200, map[string]any{"Instances": map[string]any{"Instance": []any{
					instanceJSON("i-bj", "cn-beijing", "Running", "", "172.16.0.9", 1, "NVIDIA A10"),
				}}, "NextToken": ""}
			}
		case "DescribeCloudAssistantStatus":
			return 200, map[string]any{"InstanceCloudAssistantStatusSet": map[string]any{"InstanceCloudAssistantStatus": []any{}}}
		case "ListClusters":
			return listClusters(c)
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound", "Message": c.action}
	})
}

func listedByID(t *testing.T, d *Driver) map[string]provider.Instance {
	t.Helper()
	insts, err := d.ListInstances(context.Background(), userCreds)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]provider.Instance{}
	for _, in := range insts {
		out[in.ExternalID] = in
	}
	return out
}

// E-HPC's compute nodes are ECS instances with its tag: the listing says
// which cluster each is under and the cluster's scheduler, asking E-HPC only
// in the region that has such nodes.
func TestEHPCComputeNodesNameTheirClusterAndScheduler(t *testing.T) {
	d, fake := ehpcFake(t, func(c fakeCall) (int, any) {
		if c.host != "ehpc.cn-shenzhen.aliyuncs.com" || c.query["RegionId"] != "cn-shenzhen" || c.query["PageSize"] != "50" || c.query["PageNumber"] != "1" {
			t.Errorf("ListClusters call = %+v", c)
		}
		return 200, ehpcClustersBody(
			ehpcClusterJSON("ehpc-sz-iqinzafcg4", "gpuworks-slurm-test", "slurm", "22.05.8"),
			ehpcClusterJSON("ehpc-sz-pbs", "pbs-lab", "PBS", "19.1.1"),
		)
	})
	byID := listedByID(t, d)
	got := byID["i-gpu000"].HPC
	if got == nil || *got != (provider.HPCCluster{ID: "ehpc-sz-iqinzafcg4", Name: "gpuworks-slurm-test", Scheduler: "slurm", SchedulerVersion: "22.05.8"}) {
		t.Fatalf("Slurm node HPC = %+v", got)
	}
	if got := byID["i-pbs000"].HPC; got == nil || got.Scheduler != "pbs" || got.Name != "pbs-lab" {
		t.Fatalf("PBS node HPC = %+v", got)
	}
	if byID["i-plain"].HPC != nil || byID["i-bj"].HPC != nil {
		t.Fatalf("plain instances say they are in a cluster: %+v %+v", byID["i-plain"].HPC, byID["i-bj"].HPC)
	}
	if n := strings.Count(strings.Join(fake.actions(), ","), "ListClusters"); n != 1 {
		t.Fatalf("ListClusters called %d times; want once, for the region with E-HPC nodes", n)
	}
}

// A key without E-HPC permissions still lists the account: the nodes are
// their cluster's by tag, only its name and scheduler are unknown.
func TestEHPCClustersUnreadLeaveTheNodesTheirCluster(t *testing.T) {
	d, _ := ehpcFake(t, func(fakeCall) (int, any) {
		return 403, map[string]string{"Code": "NotAuthorized", "Message": "You are not authorized by RAM for this request.", "RequestId": "req-ram"}
	})
	byID := listedByID(t, d)
	if got := byID["i-gpu000"].HPC; got == nil || *got != (provider.HPCCluster{ID: "ehpc-sz-iqinzafcg4"}) {
		t.Fatalf("node HPC = %+v; want its cluster from the tag and nothing else", got)
	}
}

// ListClusters pages until it has every cluster.
func TestEHPCClustersArePaged(t *testing.T) {
	d, fake := ehpcFake(t, func(c fakeCall) (int, any) {
		page := []map[string]any{}
		if c.query["PageNumber"] == "1" {
			for i := 0; i < ehpcPageSize; i++ {
				page = append(page, ehpcClusterJSON("ehpc-sz-other"+strings.Repeat("x", i), "other", "slurm", "22.05.8"))
			}
		} else {
			page = append(page, ehpcClusterJSON("ehpc-sz-iqinzafcg4", "late", "slurm", "22.05.8"))
		}
		body := ehpcClustersBody(page...)
		body["TotalCount"] = ehpcPageSize + 1
		return 200, body
	})
	if got := listedByID(t, d)["i-gpu000"].HPC; got == nil || got.Name != "late" {
		t.Fatalf("node HPC = %+v; want the cluster from the second page", got)
	}
	if n := strings.Count(strings.Join(fake.actions(), ","), "ListClusters"); n != 2 {
		t.Fatalf("ListClusters called %d times", n)
	}
}
