// Package kube is the agent runtime that runs instances as pods on the
// Kubernetes node the agent itself runs on (as a DaemonSet). Placement is
// already decided when a workload reaches it, so every pod is bound to this
// node by spec.nodeName and never restarted by Kubernetes: the platform's own
// reconciliation retries, as it does for docker. Images are prepared by the
// node's containerd in kubelet's namespace, logs come from the API server,
// and cards are borrowed from the kubelet when a device plugin owns them
// (cards.go).
package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/podresources"
	"gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	"gpuworks.ai/gpuworks/internal/proto"
)

// DriverName is the runtime's name, what --driver selects.
const DriverName = "kube"

// DaemonSetName is the agent DaemonSet the manifest creates.
const DaemonSetName = "gpuworks-agent"

// ImageStore is what the runtime needs of the node's containerd: nerdctl
// pinned to kubelet's "k8s.io" namespace, where pods' images live.
type ImageStore interface {
	runtime.Driver
	runtime.ImageArchiver
	runtime.ImageTagger
	runtime.ImageRemover
	runtime.ImageInspector
	runtime.LoadedRefReporter
	runtime.StreamedRefReporter
	ExecPTY(ctx context.Context, id string, cmd []string) ([]string, error)
}

// Options configure a Driver.
type Options struct {
	API          *kubeapi.Client
	Kubelet      allocationLister
	Images       ImageStore
	Namespace    string
	NodeName     string
	PodName      string // the agent's own pod
	RuntimeClass string
	// Cards maps device index to card UUID (the node's probe).
	Cards func(context.Context) (map[int]string, error)
	// IdleReturn is how long a borrowed, unused card is kept; negative keeps it.
	IdleReturn time.Duration
	Log        *slog.Logger
	// Poll is the interval of status polls (tests shorten it).
	Poll time.Duration
	// StartTimeout bounds how long Run waits for the container to start.
	StartTimeout time.Duration
}

// Driver implements runtime.Driver on Kubernetes.
type Driver struct {
	o     Options
	cards *cardPool
}

// FromEnvironment builds the driver from the pod's environment (the
// DaemonSet manifest sets it).
func FromEnvironment(cards func(context.Context) (map[int]string, error), log *slog.Logger) (*Driver, error) {
	api, err := kubeapi.InCluster()
	if err != nil {
		return nil, err
	}
	o := Options{
		API: api, Kubelet: podresources.New(podresources.DefaultSocket), Images: nerdctl.NewInNamespace("k8s.io"),
		Namespace: os.Getenv("GPUWORKS_KUBE_NAMESPACE"), NodeName: os.Getenv("GPUWORKS_KUBE_NODE_NAME"),
		PodName: os.Getenv("GPUWORKS_KUBE_POD_NAME"), RuntimeClass: os.Getenv("GPUWORKS_KUBE_RUNTIME_CLASS"),
		Cards: cards, IdleReturn: 10 * time.Minute, Log: log,
	}
	if o.Namespace == "" || o.NodeName == "" || o.PodName == "" {
		return nil, errors.New("GPUWORKS_KUBE_NAMESPACE, GPUWORKS_KUBE_NODE_NAME and GPUWORKS_KUBE_POD_NAME must be set (the DaemonSet manifest sets them)")
	}
	return New(o), nil
}

// New returns a driver.
func New(o Options) *Driver {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = 3 * time.Minute
	}
	d := &Driver{o: o}
	d.cards = &cardPool{
		api: o.API, kubelet: o.Kubelet, ns: o.Namespace, node: o.NodeName, cards: o.Cards,
		idleReturn: o.IdleReturn, now: time.Now, log: o.Log, poll: o.Poll,
	}
	return d
}

// SetIdleReturn sets how long a borrowed card outlives its last instance
// (the cluster's setting, learned at enrollment); negative keeps cards.
func (d *Driver) SetIdleReturn(v time.Duration) {
	d.cards.mu.Lock()
	d.cards.idleReturn = v
	d.cards.mu.Unlock()
}

// Name implements runtime.Driver.
func (d *Driver) Name() string { return DriverName }

// Detect reads the agent's own pod and DaemonSet: the image a hold pod runs
// and the owner every pod gets, so deleting the DaemonSet removes them.
func (d *Driver) Detect(ctx context.Context) (bool, error) {
	var self kubeapi.Pod
	if err := d.o.API.Get(ctx, kubeapi.PodPath(d.o.Namespace, d.o.PodName), &self); err != nil {
		return false, fmt.Errorf("read the agent's own pod: %w", err)
	}
	var ds kubeapi.DaemonSet
	if err := d.o.API.Get(ctx, kubeapi.DaemonSetPath(d.o.Namespace, DaemonSetName), &ds); err != nil {
		return false, fmt.Errorf("read the agent DaemonSet: %w", err)
	}
	if len(self.Spec.Containers) > 0 {
		d.cards.holdImage = self.Spec.Containers[0].Image
	}
	d.cards.holdCmd = []string{"/usr/local/bin/gpuworks-agent", "hold"}
	d.cards.owner = &kubeapi.OwnerReference{APIVersion: "apps/v1", Kind: "DaemonSet", Name: DaemonSetName, UID: ds.Metadata.UID}
	return true, nil
}

// Start borrows every free card at once when the cluster keeps cards, and
// returns idle ones on a timer otherwise. It runs until ctx ends.
func (d *Driver) Start(ctx context.Context) {
	if d.cards.idleReturn < 0 {
		if err := d.cards.keepAll(ctx); err != nil {
			d.o.Log.Warn("borrowing the node's cards failed", "err", err)
		}
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.cards.sweep(ctx)
		}
	}
}

// Image operations run in the node's containerd, kubelet's namespace.

func (d *Driver) ImageExists(ctx context.Context, image string) (bool, error) {
	return d.o.Images.ImageExists(ctx, image)
}
func (d *Driver) PullImage(ctx context.Context, image string, onLine func(string)) error {
	return d.o.Images.PullImage(ctx, image, onLine)
}
func (d *Driver) InspectImage(ctx context.Context, image string) (runtime.ImageInspection, error) {
	return d.o.Images.InspectImage(ctx, image)
}
func (d *Driver) TagImage(ctx context.Context, source, target string) error {
	return d.o.Images.TagImage(ctx, source, target)
}
func (d *Driver) RemoveImage(ctx context.Context, image string) error {
	return d.o.Images.RemoveImage(ctx, image)
}
func (d *Driver) SaveImage(ctx context.Context, image string, w io.Writer) error {
	return d.o.Images.SaveImage(ctx, image, w)
}
func (d *Driver) LoadImage(ctx context.Context, image string, r io.Reader) error {
	return d.o.Images.LoadImage(ctx, image, r)
}
func (d *Driver) LoadImageRef(ctx context.Context, image string, r io.Reader) (string, error) {
	return d.o.Images.LoadImageRef(ctx, image, r)
}
func (d *Driver) LoadImageFile(ctx context.Context, image, path string) error {
	return d.o.Images.LoadImageFile(ctx, image, path)
}
func (d *Driver) LoadImageFileRef(ctx context.Context, image, path string) (string, error) {
	return d.o.Images.LoadImageFileRef(ctx, image, path)
}

// Prepare implements runtime.Driver.
func (d *Driver) Prepare(ctx context.Context, spec runtime.WorkloadSpec) error { return nil }

func (d *Driver) instancePods(ctx context.Context) ([]kubeapi.Pod, error) {
	var pods kubeapi.PodList
	q := kubeapi.ListQuery(LabelRole+"="+RoleInstance+","+LabelNode+"="+d.o.NodeName, map[string]string{"spec.nodeName": d.o.NodeName})
	if err := d.o.API.Get(ctx, kubeapi.PodsPath(d.o.Namespace)+q, &pods); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// ListContainers implements runtime.Driver: this node's instance pods.
func (d *Driver) ListContainers(ctx context.Context) ([]string, error) {
	pods, err := d.instancePods(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pods))
	for _, p := range pods {
		out = append(out, p.Metadata.Name)
	}
	return out, nil
}

// RunningContainers implements runtime.Driver.
func (d *Driver) RunningContainers(ctx context.Context) ([]string, error) {
	pods, err := d.instancePods(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pods {
		if running(p) {
			out = append(out, p.Metadata.Name)
		}
	}
	return out, nil
}

func engineStatus(p kubeapi.Pod) *kubeapi.ContainerStatus {
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == ContainerName {
			return &p.Status.ContainerStatuses[i]
		}
	}
	return nil
}

func running(p kubeapi.Pod) bool {
	cs := engineStatus(p)
	return p.Metadata.DeletionTimestamp == nil && cs != nil && cs.State.Running != nil
}

// startFailure is the reason a pod will not start, or "".
func startFailure(p kubeapi.Pod) string {
	if p.Status.Phase == "Failed" {
		return strings.TrimSpace(p.Status.Reason + ": " + p.Status.Message)
	}
	cs := engineStatus(p)
	if cs == nil || cs.State.Waiting == nil {
		return ""
	}
	switch cs.State.Waiting.Reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "ErrImageNeverPull":
		return cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message
	}
	return ""
}

// Run implements runtime.Driver: borrow the cards, create the pod and wait
// until its container runs (or exited, which the engine then reports).
func (d *Driver) Run(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	cards, err := d.cards.acquire(ctx, spec.Name, spec.GPUIndexes)
	if err != nil {
		return "", err
	}
	pod, err := instancePod(spec, podOptions{Namespace: d.o.Namespace, NodeName: d.o.NodeName, RuntimeClass: d.o.RuntimeClass, Owner: d.cards.owner, Cards: cards})
	if err != nil {
		d.cards.release(spec.Name)
		return "", err
	}
	if err := d.o.API.Create(ctx, kubeapi.PodsPath(d.o.Namespace), pod, nil); err != nil {
		d.cards.release(spec.Name)
		return "", fmt.Errorf("create pod %s: %w", spec.Name, err)
	}
	deadline := time.Now().Add(d.o.StartTimeout)
	for {
		var p kubeapi.Pod
		if err := d.o.API.Get(ctx, kubeapi.PodPath(d.o.Namespace, spec.Name), &p); err != nil {
			return spec.Name, fmt.Errorf("read pod %s: %w", spec.Name, err)
		}
		if cs := engineStatus(p); cs != nil && (cs.State.Running != nil || cs.State.Terminated != nil) {
			return spec.Name, nil
		}
		if why := startFailure(p); why != "" {
			_ = d.Remove(context.WithoutCancel(ctx), spec.Name)
			return "", fmt.Errorf("pod %s did not start: %s", spec.Name, why)
		}
		if time.Now().After(deadline) {
			_ = d.Remove(context.WithoutCancel(ctx), spec.Name)
			return "", fmt.Errorf("pod %s did not start within %s (phase %s)", spec.Name, d.o.StartTimeout, p.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return spec.Name, ctx.Err()
		case <-time.After(d.o.Poll):
		}
	}
}

// Stop implements runtime.Driver. A pod cannot be stopped and kept, so this
// deletes it gracefully; Remove then finds nothing to do.
func (d *Driver) Stop(ctx context.Context, id string) error {
	grace := int64(30)
	if err := d.o.API.Delete(ctx, kubeapi.PodPath(d.o.Namespace, id), &grace); err != nil {
		return err
	}
	return d.waitGone(ctx, id, 45*time.Second)
}

// Remove implements runtime.Driver.
func (d *Driver) Remove(ctx context.Context, id string) error {
	zero := int64(0)
	err := d.o.API.Delete(ctx, kubeapi.PodPath(d.o.Namespace, id), &zero)
	if err == nil {
		err = d.waitGone(ctx, id, 30*time.Second)
	}
	if err == nil {
		d.cards.release(id)
	}
	return err
}

func (d *Driver) waitGone(ctx context.Context, id string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		var p kubeapi.Pod
		err := d.o.API.Get(ctx, kubeapi.PodPath(d.o.Namespace, id), &p)
		if kubeapi.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pod %s is still terminating after %s", id, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.o.Poll):
		}
	}
}

// Status implements runtime.Driver.
func (d *Driver) Status(ctx context.Context, id string) (runtime.Status, error) {
	var p kubeapi.Pod
	err := d.o.API.Get(ctx, kubeapi.PodPath(d.o.Namespace, id), &p)
	if kubeapi.IsNotFound(err) {
		return runtime.Status{}, nil
	}
	if err != nil {
		return runtime.Status{}, err
	}
	st := runtime.Status{Exists: true, Running: running(p)}
	if cs := engineStatus(p); cs != nil && cs.State.Terminated != nil {
		st.ExitCode = cs.State.Terminated.ExitCode
		st.OOMKilled = cs.State.Terminated.Reason == "OOMKilled"
	} else if p.Status.Phase == "Failed" {
		st.ExitCode = 1 // evicted or refused by the kubelet
	}
	return st, nil
}

// Logs implements runtime.Driver.
func (d *Driver) Logs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	q := url.Values{"container": {ContainerName}}
	if follow {
		q.Set("follow", "true")
	}
	if tail > 0 {
		q.Set("tailLines", strconv.Itoa(tail))
	}
	return d.o.API.Stream(ctx, kubeapi.PodPath(d.o.Namespace, id)+"/log?"+q.Encode())
}

// containerID is the pod's engine container in containerd.
func (d *Driver) containerID(ctx context.Context, id string) (string, error) {
	var p kubeapi.Pod
	if err := d.o.API.Get(ctx, kubeapi.PodPath(d.o.Namespace, id), &p); err != nil {
		return "", err
	}
	cs := engineStatus(p)
	if cs == nil || cs.ContainerID == "" {
		return "", fmt.Errorf("pod %s has no running container", id)
	}
	_, cid, _ := strings.Cut(cs.ContainerID, "://")
	return cid, nil
}

// Exec implements runtime.Driver, through the node's containerd.
func (d *Driver) Exec(ctx context.Context, id string, cmd []string) ([]byte, error) {
	cid, err := d.containerID(ctx, id)
	if err != nil {
		return nil, err
	}
	return d.o.Images.Exec(ctx, cid, cmd)
}

// ExecPTY implements runtime.PTYExecer.
func (d *Driver) ExecPTY(ctx context.Context, id string, cmd []string) ([]string, error) {
	cid, err := d.containerID(ctx, id)
	if err != nil {
		return nil, err
	}
	return d.o.Images.ExecPTY(ctx, cid, cmd)
}

// Unschedulable reports whether this node is cordoned in Kubernetes.
func (d *Driver) Unschedulable(ctx context.Context) (bool, error) {
	var n kubeapi.Node
	if err := d.o.API.Get(ctx, kubeapi.NodePath(d.o.NodeName)+"?resourceVersion=0", &n); err != nil {
		return false, err
	}
	return n.Spec.Unschedulable, nil
}

// ExternalCardHolders maps card UUID to the pods outside the platform the
// kubelet gave it to; the agent reports them as the card's foreign use.
func (d *Driver) ExternalCardHolders(ctx context.Context) (map[string][]proto.CardProcess, error) {
	if d.o.Kubelet == nil {
		return nil, nil
	}
	return d.cards.externalHolders(ctx)
}

var (
	_ runtime.Driver              = (*Driver)(nil)
	_ runtime.PTYExecer           = (*Driver)(nil)
	_ runtime.ImageArchiver       = (*Driver)(nil)
	_ runtime.ImageTagger         = (*Driver)(nil)
	_ runtime.ImageRemover        = (*Driver)(nil)
	_ runtime.ImageInspector      = (*Driver)(nil)
	_ runtime.LoadedRefReporter   = (*Driver)(nil)
	_ runtime.StreamedRefReporter = (*Driver)(nil)
	_ ImageStore                  = (*nerdctl.Driver)(nil)
)
