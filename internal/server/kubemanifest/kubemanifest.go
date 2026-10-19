// Package kubemanifest renders the manifest a Kubernetes cluster applies to
// join: one DaemonSet whose agents trade the cluster's enrollment token for
// node tokens, plus the namespace, identity and permissions they need. It is
// a pure function of its parameters, with no knowledge of the store or of
// how the manifest is served.
package kubemanifest

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// Names every rendered object uses. The agent finds its own DaemonSet and
// the instance pods' PriorityClass by them.
const (
	AgentName         = "gpuworks-agent"
	EnrollSecret      = "gpuworks-enroll"
	PriorityClassName = "gpuworks-inference"
	// StateDir is where the agent keeps its state on the host. It is mounted
	// at the same path inside the agent so every path the agent hands an
	// instance pod is a host path as well.
	StateDir = "/opt/gpuworks"
	// PodResourcesDir is kubelet's PodResources socket directory.
	PodResourcesDir = "/var/lib/kubelet/pod-resources"
)

// Params are a cluster's manifest parameters.
type Params struct {
	ClusterName      string
	Namespace        string
	ServerURL        string
	Token            string
	AgentImage       string
	NodeSelector     map[string]string
	RuntimeClass     string
	ContainerdSocket string
	EnginePorts      string
}

var (
	dnsLabel  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	labelPart = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
)

// ParseNodeSelector reads "key=value,key2=value2" into a selector.
func ParseNodeSelector(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		name := k
		if i := strings.LastIndex(k, "/"); i >= 0 {
			name = k[i+1:]
		}
		if !ok || len(name) > 63 || !labelPart.MatchString(name) || len(v) > 63 || (v != "" && !labelPart.MatchString(v)) {
			return nil, fmt.Errorf("node selector %q: want key=value label pairs", part)
		}
		out[k] = v
	}
	return out, nil
}

// Validate reports the first parameter the manifest cannot be rendered with.
func (p Params) Validate() error {
	switch {
	case len(p.Namespace) > 63 || !dnsLabel.MatchString(p.Namespace):
		return fmt.Errorf("namespace %q is not a DNS label", p.Namespace)
	case !strings.HasPrefix(p.ServerURL, "http://") && !strings.HasPrefix(p.ServerURL, "https://"):
		return fmt.Errorf("server URL %q must start with http:// or https://", p.ServerURL)
	case strings.TrimSpace(p.Token) == "":
		return fmt.Errorf("enrollment token is empty")
	case strings.TrimSpace(p.AgentImage) == "":
		return fmt.Errorf("agent image is empty")
	case p.RuntimeClass != "" && (len(p.RuntimeClass) > 63 || !dnsLabel.MatchString(p.RuntimeClass)):
		return fmt.Errorf("runtime class %q is not a DNS label", p.RuntimeClass)
	case !strings.HasPrefix(p.ContainerdSocket, "/"):
		return fmt.Errorf("containerd socket %q must be an absolute path", p.ContainerdSocket)
	}
	return nil
}

// Render returns the manifest as one multi-document YAML stream.
func Render(p Params) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(p.NodeSelector))
	for k := range p.NodeSelector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type kv struct{ K, V string }
	selector := make([]kv, 0, len(keys))
	for _, k := range keys {
		selector = append(selector, kv{k, p.NodeSelector[k]})
	}
	var buf bytes.Buffer
	err := manifest.Execute(&buf, map[string]any{
		"P": p, "Selector": selector, "Agent": AgentName, "Secret": EnrollSecret,
		"Priority": PriorityClassName, "StateDir": StateDir, "PodResources": PodResourcesDir,
	})
	return buf.String(), err
}

// q quotes a value as a YAML double-quoted scalar (JSON string syntax is a
// subset of it), so no parameter can change the document's structure.
func q(s string) string { return strconv.Quote(s) }

var manifest = template.Must(template.New("manifest").Funcs(template.FuncMap{"q": q}).Parse(`# GPUWorks agents for cluster {{q .P.ClusterName}}.
# Apply with kubectl apply -f; delete the same file to remove every agent and
# instance pod GPUWorks started (the pods are owned by the DaemonSet).
apiVersion: v1
kind: Namespace
metadata:
  name: {{q .P.Namespace}}
  labels:
    pod-security.kubernetes.io/enforce: privileged
    app.kubernetes.io/managed-by: gpuworks
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{.Agent}}
  namespace: {{q .P.Namespace}}
---
apiVersion: v1
kind: Secret
metadata:
  name: {{.Secret}}
  namespace: {{q .P.Namespace}}
type: Opaque
stringData:
  token: {{q .P.Token}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{.Agent}}
  namespace: {{q .P.Namespace}}
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch", "create", "delete", "patch"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: ["apps"]
    resources: ["daemonsets"]
    resourceNames: ["{{.Agent}}"]
    verbs: ["get", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{.Agent}}
  namespace: {{q .P.Namespace}}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{.Agent}}
subjects:
  - kind: ServiceAccount
    name: {{.Agent}}
    namespace: {{q .P.Namespace}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{q (printf "%s-%s" .Agent .P.Namespace)}}
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["namespaces"]
    resourceNames: ["kube-system"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{q (printf "%s-%s" .Agent .P.Namespace)}}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{q (printf "%s-%s" .Agent .P.Namespace)}}
subjects:
  - kind: ServiceAccount
    name: {{.Agent}}
    namespace: {{q .P.Namespace}}
---
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: {{.Priority}}
value: 100000
globalDefault: false
description: "GPUWorks inference instances: evicted after ordinary workloads."
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: {{.Agent}}
  namespace: {{q .P.Namespace}}
  labels:
    app.kubernetes.io/name: {{.Agent}}
    app.kubernetes.io/managed-by: gpuworks
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: {{.Agent}}
  updateStrategy:
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/name: {{.Agent}}
        app.kubernetes.io/managed-by: gpuworks
    spec:
      serviceAccountName: {{.Agent}}
      hostNetwork: true
      hostPID: true
      dnsPolicy: ClusterFirstWithHostNet
      priorityClassName: system-node-critical
{{- if .P.RuntimeClass}}
      runtimeClassName: {{q .P.RuntimeClass}}
{{- end}}
{{- if .Selector}}
      nodeSelector:
{{- range .Selector}}
        {{q .K}}: {{q .V}}
{{- end}}
{{- end}}
      tolerations:
        - operator: Exists
      terminationGracePeriodSeconds: 30
      containers:
        - name: agent
          image: {{q .P.AgentImage}}
          imagePullPolicy: IfNotPresent
          args:
            - run
            - {{q (printf "--server=%s" .P.ServerURL)}}
            - "--state={{.StateDir}}"
            - "--driver=kube"
{{- if .P.EnginePorts}}
            - {{q (printf "--engine-ports=%s" .P.EnginePorts)}}
{{- end}}
          env:
            - name: GPUWORKS_KUBE_ENROLL_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{.Secret}}
                  key: token
            - name: GPUWORKS_KUBE_NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: GPUWORKS_KUBE_POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: GPUWORKS_KUBE_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: GPUWORKS_KUBE_RUNTIME_CLASS
              value: {{q .P.RuntimeClass}}
            - name: CONTAINERD_ADDRESS
              value: {{q .P.ContainerdSocket}}
            - name: NVIDIA_VISIBLE_DEVICES
              value: "all"
            - name: NVIDIA_DRIVER_CAPABILITIES
              value: "compute,utility"
          securityContext:
            privileged: true
          volumeMounts:
            - name: state
              mountPath: {{.StateDir}}
              mountPropagation: Bidirectional
            - name: containerd
              mountPath: {{q .P.ContainerdSocket}}
            - name: pod-resources
              mountPath: {{.PodResources}}
              readOnly: true
      volumes:
        - name: state
          hostPath:
            path: {{.StateDir}}
            type: DirectoryOrCreate
        - name: containerd
          hostPath:
            path: {{q .P.ContainerdSocket}}
            type: Socket
        - name: pod-resources
          hostPath:
            path: {{.PodResources}}
            type: DirectoryOrCreate
`))
