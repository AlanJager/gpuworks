package kube

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

func TestTranslateDockerArgs(t *testing.T) {
	tr, err := translateDockerArgs([]string{"--gpus", `"device=1,3"`, "--ipc=host", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK",
		"--shm-size=8g", "--group-add", "44", "-e", "A=b", "--network=host", "--device=/dev/infiniband"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Env["NVIDIA_VISIBLE_DEVICES"] != "1,3" || tr.Env["A"] != "b" || tr.RuntimeClass != "nvidia" || !tr.HostIPC || !tr.Privileged ||
		len(tr.CapAdd) != 1 || tr.CapAdd[0] != "IPC_LOCK" || tr.ShmBytes != 8<<30 || len(tr.Groups) != 1 || tr.Groups[0] != 44 {
		t.Fatalf("translation = %+v", tr)
	}
	tr, err = translateDockerArgs([]string{"--runtime", "ascend", "--pid", "host"})
	if err != nil || tr.RuntimeClass != "ascend" || !tr.HostPID || tr.Privileged {
		t.Fatalf("ascend translation = %+v, %v", tr, err)
	}
	for _, bad := range [][]string{{"--memory=1g"}, {"--network", "bridge"}, {"--group-add", "video"}, {"--gpus"}, {"positional"}, {"--shm-size", "lots"}} {
		if _, err := translateDockerArgs(bad); err == nil {
			t.Fatalf("translateDockerArgs(%q) accepted", bad)
		}
	}
}

func TestInstancePod(t *testing.T) {
	spec := runtime.WorkloadSpec{
		Name: "gpuworks-0123456789ab", Image: "vllm/vllm-openai:v0.11", Entrypoint: []string{"vllm"}, Cmd: []string{"serve", "/model"},
		Env:        map[string]string{"Z": "1", "CUDA_VISIBLE_DEVICES": "0"},
		Mounts:     []runtime.Mount{{Source: "/opt/gpuworks/models/qwen", Target: "/model", ReadOnly: true}},
		DockerArgs: []string{"--gpus", `"device=2"`}, HostNetwork: true, ShmBytes: 1 << 30,
	}
	owner := &kubeapi.OwnerReference{APIVersion: "apps/v1", Kind: "DaemonSet", Name: DaemonSetName, UID: "ds"}
	pod, err := instancePod(spec, podOptions{Namespace: "gw", NodeName: "n1", RuntimeClass: "nvidia-cdi", Owner: owner, Cards: []string{"GPU-2"}})
	if err != nil {
		t.Fatal(err)
	}
	s := pod.Spec
	c := s.Containers[0]
	if pod.Metadata.Name != spec.Name || pod.Metadata.Namespace != "gw" || pod.Metadata.Labels[LabelRole] != RoleInstance || pod.Metadata.Labels[LabelNode] != "n1" ||
		pod.Metadata.Annotations[AnnotationCards] != "GPU-2" || len(pod.Metadata.OwnerReferences) != 1 {
		t.Fatalf("metadata = %+v", pod.Metadata)
	}
	if s.NodeName != "n1" || s.RestartPolicy != "Never" || !s.HostNetwork || s.DNSPolicy != "ClusterFirstWithHostNet" ||
		s.RuntimeClassName == nil || *s.RuntimeClassName != "nvidia-cdi" || s.PriorityClassName != "gpuworks-inference" {
		t.Fatalf("spec = %+v", s)
	}
	if c.Name != ContainerName || c.Image != spec.Image || strings.Join(c.Command, " ") != "vllm" || strings.Join(c.Args, " ") != "serve /model" || c.SecurityContext != nil {
		t.Fatalf("container = %+v", c)
	}
	var env []string
	for _, e := range c.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	if strings.Join(env, " ") != "CUDA_VISIBLE_DEVICES=0 NVIDIA_VISIBLE_DEVICES=2 Z=1" {
		t.Fatalf("env = %v", env)
	}
	if len(s.Volumes) != 2 || s.Volumes[0].HostPath.Path != "/opt/gpuworks/models/qwen" || !c.VolumeMounts[0].ReadOnly ||
		s.Volumes[1].EmptyDir == nil || s.Volumes[1].EmptyDir.Medium != "Memory" || s.Volumes[1].EmptyDir.SizeLimit != "1073741824" || c.VolumeMounts[1].MountPath != "/dev/shm" {
		t.Fatalf("volumes = %+v mounts = %+v", s.Volumes, c.VolumeMounts)
	}
	// No NVIDIA runtime class configured: the default runtime is NVIDIA's.
	pod, _ = instancePod(spec, podOptions{Namespace: "gw", NodeName: "n1"})
	if pod.Spec.RuntimeClassName != nil {
		t.Fatalf("runtime class without one configured = %v", *pod.Spec.RuntimeClassName)
	}
	spec.DockerArgs = []string{"--memory", "1g"}
	if _, err := instancePod(spec, podOptions{}); err == nil {
		t.Fatal("an untranslatable docker option was dropped")
	}
}
