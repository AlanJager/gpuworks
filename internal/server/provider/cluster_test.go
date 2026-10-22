package provider

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// clusterTestDriver is an account with one managed cluster whose node is
// also one of the account's instances.
type clusterTestDriver struct {
	serviceTestDriver
	kubeconfig    []byte
	kubeconfigErr error
	mu            sync.Mutex
	asked         []string
}

func (*clusterTestDriver) Name() string { return "cluster-test-provider" }

func (*clusterTestDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	return []Instance{{ExternalID: "i-node", Name: "ack-node", PublicIP: "203.0.113.20", Cluster: "c-1"},
		{ExternalID: "i-plain", Name: "plain", PublicIP: "203.0.113.21"}}, nil
}

func (*clusterTestDriver) ListClusters(context.Context, Credentials) ([]Cluster, error) {
	return []Cluster{{ExternalID: "c-1", Name: "ack", Region: "cn-shanghai", Running: true, PublicAPI: true,
		GPUPools: []ClusterPool{{ID: "np-1", Name: "gpu", GPUModel: "L20", GPUCount: 1, Nodes: 1, Vendor: "nvidia", NodeSelector: "alibabacloud.com/nodepool-id=np-1"}}}}, nil
}

func (d *clusterTestDriver) ClusterKubeconfig(_ context.Context, _ Credentials, id string) ([]byte, error) {
	d.mu.Lock()
	d.asked = append(d.asked, id)
	d.mu.Unlock()
	return d.kubeconfig, d.kubeconfigErr
}

func clusterService(t *testing.T, drv Driver) (*Service, *store.Provider) {
	t.Helper()
	Register(drv)
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("cluster-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	p, err := svc.Add(context.Background(), drv.Name(), "acct", Credentials{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, p
}

// acceptingAPI is an API server where nothing exists yet and every apply is
// accepted, except paths in refuse.
func acceptingAPI(t *testing.T, refuse string) []byte {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch {
		case refuse != "" && strings.Contains(r.URL.Path, refuse) && r.Method == http.MethodPatch:
			w.WriteHeader(403)
			fmt.Fprint(w, `{"kind":"Status","status":"Failure","reason":"Forbidden","message":"forbidden by RBAC","code":403}`)
		case r.Method == http.MethodGet:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return []byte(fmt.Sprintf("current-context: c\nclusters: [{name: c, cluster: {server: %q, certificate-authority-data: %s}}]\ncontexts: [{name: c, context: {cluster: c, user: u}}]\nusers: [{name: u, user: {token: tok}}]\n", srv.URL, ca))
}

func joinManifest(t *testing.T) string {
	t.Helper()
	m, err := kubemanifest.Render(kubemanifest.Params{ClusterName: "ack", Namespace: "gpuworks-system", ServerURL: "https://gw.example", Token: "x", AgentImage: "a:1", Vendor: "nvidia"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestApplyToClusterUsesAFreshKubeconfigAndReportsEachObject(t *testing.T) {
	drv := &clusterTestDriver{}
	drv.kubeconfig = acceptingAPI(t, "")
	svc, p := clusterService(t, drv)
	ctx := context.Background()
	clusters, err := svc.Clusters(ctx, p.ID)
	if err != nil || len(clusters) != 1 || clusters[0].GPUPools[0].NodeSelector != "alibabacloud.com/nodepool-id=np-1" {
		t.Fatalf("Clusters = %+v, %v", clusters, err)
	}
	results, err := svc.ApplyToCluster(ctx, p.ID, "c-1", joinManifest(t), false)
	if err != nil || len(results) != 9 || results[8].Kind != "DaemonSet" || results[8].Action != "create" {
		t.Fatalf("ApplyToCluster = %+v, %v", results, err)
	}
	if _, err := svc.ApplyToCluster(ctx, p.ID, "c-1", joinManifest(t), true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(drv.asked, ",") != "c-1,c-1" {
		t.Fatalf("kubeconfigs issued for %v; each operation asks for its own", drv.asked)
	}
}

func TestClusterOperationErrorsSayWhichStepFailed(t *testing.T) {
	ctx := context.Background()
	drv := &clusterTestDriver{kubeconfigErr: errors.New("aliyun: ErrorClusterNotFound")}
	svc, p := clusterService(t, drv)
	var opErr *ClusterOperationError
	if _, err := svc.ApplyToCluster(ctx, p.ID, "c-1", joinManifest(t), true); !errors.As(err, &opErr) || opErr.Step != "kubeconfig" {
		t.Fatalf("kubeconfig failure = %v", err)
	}
	drv.kubeconfigErr, drv.kubeconfig = nil, []byte("users: [{name: u, user: {exec: {command: aliyun}}}]\ncontexts: [{name: c, context: {cluster: c, user: u}}]\nclusters: [{name: c, cluster: {server: 'https://a'}}]\ncurrent-context: c\n")
	if _, err := svc.RemoveFromCluster(ctx, p.ID, "c-1", joinManifest(t), true); !errors.As(err, &opErr) || opErr.Step != "kubeconfig" {
		t.Fatalf("unusable kubeconfig = %v", err)
	}
	drv.kubeconfig = acceptingAPI(t, "clusterroles")
	results, err := svc.ApplyToCluster(ctx, p.ID, "c-1", joinManifest(t), false)
	if !errors.As(err, &opErr) || opErr.Step != "api_server" || !strings.Contains(err.Error(), "forbidden by RBAC") {
		t.Fatalf("refusal = %v", err)
	}
	if results[5].Action != "failed" || results[8].Action != "not_tried" {
		t.Fatalf("results with a refusal = %+v", results)
	}
	if _, err := svc.ApplyToCluster(ctx, p.ID, "c-1", "kind: ConfigMap\nmetadata: {name: x}\n", false); err == nil || errors.As(err, &opErr) {
		t.Fatalf("a manifest of other objects = %v", err)
	}
}

func TestAProviderWithoutClustersSaysSo(t *testing.T) {
	svc, p := clusterService(t, serviceTestDriver{})
	if _, err := svc.Clusters(context.Background(), p.ID); !errors.Is(err, ErrNoClusters) || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Clusters = %v", err)
	}
	if _, err := svc.ApplyToCluster(context.Background(), p.ID, "c", joinManifest(t), true); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ApplyToCluster = %v", err)
	}
	for _, info := range KindInfos() {
		if info.Kind == "service-test-provider" && info.CanListClusters {
			t.Fatal("service-test-provider claims clusters")
		}
	}
}

func TestAClusterNodeIsRecordedAndNotInstalledOnItsOwn(t *testing.T) {
	drv := &clusterTestDriver{}
	svc, p := clusterService(t, drv)
	ctx := context.Background()
	if _, err := svc.Sync(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.store.ProviderInstances(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	managed := map[string]string{}
	for _, r := range rows {
		managed[r.ExternalID] = r.ManagedCluster
	}
	if managed["i-node"] != "c-1" || managed["i-plain"] != "" {
		t.Fatalf("managed clusters = %v", managed)
	}
	if _, err := svc.Install(ctx, p.ID, "i-node", "https://gw.example", InstallOptions{}); !errors.Is(err, ErrInstanceInCluster) {
		t.Fatalf("install on a cluster node = %v", err)
	}
	if _, err := svc.Precheck(ctx, p.ID, "i-node", "https://gw.example", InstallOptions{}); !errors.Is(err, ErrInstanceInCluster) {
		t.Fatalf("precheck on a cluster node = %v", err)
	}
	found := false
	for _, info := range KindInfos() {
		if info.Kind == "cluster-test-provider" {
			found = info.CanListClusters
		}
	}
	if !found {
		t.Fatal("KindInfos does not say the driver lists clusters")
	}
}
