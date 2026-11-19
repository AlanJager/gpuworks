package kube

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// HAMi's EncodeNodeDevices historically writes seven comma-separated fields
// per card and terminates each card with a colon. A newer plugin writes JSON.
// Both reach the driver through the Node API, not a local parser-only seam.
func TestHAMiLegacyRegistrationKeepsRealHoldersAndBorrowsFreeCard(t *testing.T) {
	r, _ := newHAMiRig(t, 2)
	r.srv.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register",
		"GPU-a,10,46068,100,NVIDIA L20,0,true:GPU-b,10,46068,100,NVIDIA L20,0,true:")
	r.externalPod(t, "ml", "partial", DefaultHAMiScheduler, map[string]string{"nvidia.com/use-gpuuuid": "GPU-a"},
		map[string]string{"nvidia.com/gpu": "1", "nvidia.com/gpucores": "30", "nvidia.com/gpumem": "8000"})
	held, err := r.d.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(held["GPU-b"]) != 0 || len(held["GPU-a"]) != 1 || held["GPU-a"][0].MemoryBytes != 8000<<20 || !strings.Contains(held["GPU-a"][0].Name, "30%") {
		t.Fatalf("holders = %+v; want only the real 8000 MiB partial holder of GPU-a", held)
	}
	if _, err := r.d.Run(context.Background(), spec("gpuworks-bbbbbbbbbbbb", 0)); err == nil || !strings.Contains(err.Error(), "ml/partial") {
		t.Fatalf("borrow genuinely occupied GPU-a = %v", err)
	}
	if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 1)); err != nil {
		t.Fatalf("borrow free card GPU-b: %v", err)
	}
	if holds := r.holdPods(); len(holds) != 1 || r.srv.Pod(testNS, holds[0]).Metadata.Annotations["nvidia.com/use-gpuuuid"] != "GPU-b" {
		t.Fatalf("hold pods = %+v; want only GPU-b", holds)
	}
}

func TestHAMiRegistrationFormatsBorrowTheSameWholeCard(t *testing.T) {
	for name, registration := range map[string]string{
		"legacy seven fields": "GPU-a,10,46068,100,NVIDIA L20,0,true:",
		"legacy nine fields":  "GPU-a,10,46068,100,NVIDIA L20,0,true,0,hami-core:",
		"JSON":                `[{"id":"GPU-a","count":10,"devmem":46068,"devcore":100,"type":"NVIDIA L20","health":true,"index":0,"mode":"hami-core"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := newHAMiRig(t, 1)
			r.srv.UpdateNode(testNode, func(n *kubeapi.Node) {
				n.Metadata.Annotations["hami.io/node-nvidia-register"] = registration
			})
			held, err := r.d.ExternalCardHolders(context.Background())
			if err != nil || len(held) != 0 {
				t.Fatalf("free card holders = %v, %v", held, err)
			}
			if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
				t.Fatalf("borrow registered whole card: %v", err)
			}
		})
	}
}

func TestHAMiLegacyRegistrationDoesNotBorrowUnhealthyCard(t *testing.T) {
	r, _ := newHAMiRig(t, 2)
	r.srv.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register",
		"GPU-a,10,46068,100,NVIDIA L20,0,true:GPU-b,10,46068,100,NVIDIA L20,0,false:")
	r.srv.SetCapacity(testNode, resNVIDIAGPU, "10", "10")
	if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 1)); err == nil || !strings.Contains(err.Error(), "does not list card GPU-b as a healthy card") {
		t.Fatalf("borrow unhealthy GPU-b = %v", err)
	}
	if len(r.holdPods()) != 0 {
		t.Fatal("created hold pod for unhealthy GPU-b")
	}
	if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatalf("borrow healthy GPU-a: %v", err)
	}
}

// Broken registration must stay visible as unknown ownership. In particular,
// a good first record must not hide a malformed or missing second record.
func TestHAMiInvalidRegistrationNeverReportsCardsFree(t *testing.T) {
	for name, registration := range map[string]string{
		"empty":              "",
		"JSON null":          "null",
		"JSON empty":         "[]",
		"partial JSON":       `[{"id":"GPU-a","count":20,"health":true}]`,
		"partial record":     "GPU-a,10,46068,100,NVIDIA L20,0,true:GPU-b,10,46068,100,NVIDIA L20,0:",
		"unreadable count":   "GPU-a,twenty,46068,100,NVIDIA L20,0,true:",
		"negative memory":    "GPU-a,20,-1,100,NVIDIA L20,0,true:",
		"unreadable health":  "GPU-a,20,46068,100,NVIDIA L20,0,maybe:",
		"unreadable NUMA":    "GPU-a,20,46068,100,NVIDIA L20,wrong,true:",
		"unreadable index":   "GPU-a,20,46068,100,NVIDIA L20,0,true,wrong,hami-core:",
		"duplicate identity": "GPU-a,10,46068,100,NVIDIA L20,0,true:GPU-a,10,46068,100,NVIDIA L20,0,true:",
		"missing card":       "GPU-a,10,46068,100,NVIDIA L20,0,true:",
		"eight fields":       "GPU-a,20,46068,100,NVIDIA L20,0,true,0:",
		"MIG mode":           "GPU-a,20,46068,100,NVIDIA L20,0,true,0,mig:",
		"remote JSON mode":   `[{"id":"GPU-a","count":20,"devmem":46068,"devcore":100,"health":true,"mode":"remote"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			r, s := newHAMiRig(t, 2)
			r.srv.UpdateNode(testNode, func(n *kubeapi.Node) {
				n.Metadata.Annotations["hami.io/node-nvidia-register"] = registration
			})
			held, err := r.d.ExternalCardHolders(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, card := range []string{"GPU-a", "GPU-b"} {
				if len(held[card]) != 1 || !strings.Contains(held[card][0].Name, "HAMi") {
					t.Fatalf("%s holders = %+v, want unknown registration reason", card, held[card])
				}
			}
			if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 0)); err == nil {
				t.Fatal("borrow succeeded with an invalid registration")
			}
			if len(s.holdCreated()) != 0 {
				t.Fatal("created a hold pod despite unknown registration")
			}
		})
	}
}
