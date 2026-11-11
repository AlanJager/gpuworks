package accelerator

import "testing"

func TestClassifyKubeResource(t *testing.T) {
	for _, c := range []struct {
		resource string
		kind     KubeResourceKind
		vendor   string
	}{
		{"nvidia.com/gpu", KubeResourceCard, VendorNVIDIA},
		{"nvidia.com/gpu.shared", KubeResourceShared, VendorNVIDIA},
		{"nvidia.com/mig-1g.10gb", KubeResourceShared, VendorNVIDIA},
		{"nvidia.com/gpumem", KubeResourceShared, VendorNVIDIA},
		{"nvidia.com/gpucores", KubeResourceShared, VendorNVIDIA},
		{"huawei.com/Ascend910", KubeResourceCard, VendorAscend},
		{"huawei.com/Ascend310P", KubeResourceCard, VendorAscend},
		{"huawei.com/npu", KubeResourceCard, VendorAscend},
		{"huawei.com/Ascend910-2c", KubeResourceShared, VendorAscend},
		{"huawei.com/Ascend310P-4c.3cpu", KubeResourceShared, VendorAscend},
		{"huawei.com/npu.sliced", KubeResourceShared, VendorAscend},
		{"hygon.com/dcu", KubeResourceCard, VendorHygon},
		{"hygon.com/dcunum", KubeResourceShared, VendorHygon},
		{"amd.com/gpu", KubeResourceCard, VendorAMD},
		{"aliyun.com/gpu-mem", KubeResourceShared, VendorNVIDIA},
		{"aliyun.com/gpu-count", KubeResourceShared, VendorNVIDIA},
		{"aliyun.com/gpu-core.percentage", KubeResourceShared, VendorNVIDIA},
		{"aliyun.com/other", KubeResourceOther, ""},
		{"alibabacloud.com/ppu", KubeResourceCard, VendorPPU},
		{"alibabacloud.com/ppu.partitioned.mig-1g", KubeResourceShared, VendorPPU},
		{"cpu", KubeResourceOther, ""},
		{"example.com/gpu", KubeResourceOther, ""},
		{"nvidia.com/other", KubeResourceOther, ""},
	} {
		kind, vendor := ClassifyKubeResource(c.resource)
		if kind != c.kind || vendor != c.vendor {
			t.Errorf("ClassifyKubeResource(%q) = %v %q, want %v %q", c.resource, kind, vendor, c.kind, c.vendor)
		}
	}
}

func TestKubeProfiles(t *testing.T) {
	for _, v := range KubeVendors() {
		p, ok := KubeProfileFor(v)
		if !ok || p.Vendor != v {
			t.Fatalf("profile %q = %+v", v, p)
		}
		for _, d := range append(append([]string{}, p.HostDirs...), p.HostFiles...) {
			if d == "" || d[0] != '/' {
				t.Fatalf("%s: host path %q is not absolute", v, d)
			}
		}
	}
	if _, ok := KubeProfileFor("apple"); ok {
		t.Fatal("apple has no Kubernetes profile")
	}
	if p, _ := KubeProfileFor("NVIDIA"); !p.RuntimeClass {
		t.Fatal("NVIDIA agents run under the NVIDIA RuntimeClass")
	}
}
