package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// What the agent answers about its node and cluster, rather than about an
// instance: cordon state, the DaemonSet's upgrade, and whether a node of the
// cluster still exists.

// Unschedulable reports whether this node is cordoned in Kubernetes.
func (d *Driver) Unschedulable(ctx context.Context) (bool, error) {
	var n kubeapi.Node
	if err := d.o.API.Get(ctx, kubeapi.NodePath(d.o.NodeName)+"?resourceVersion=0", &n); err != nil {
		return false, err
	}
	return n.Spec.Unschedulable, nil
}

// UpgradeDaemonSet moves the agent DaemonSet to version: the same image
// repository at that tag. Kubernetes then replaces every agent of the
// cluster in turn; the instance pods are not the agents' and keep running.
func (d *Driver) UpgradeDaemonSet(ctx context.Context, version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" || strings.ContainsAny(version, "/@: ") {
		return "", fmt.Errorf("version %q is not an image tag", version)
	}
	var ds kubeapi.DaemonSet
	if err := d.o.API.Get(ctx, kubeapi.DaemonSetPath(d.o.Namespace, DaemonSetName), &ds); err != nil {
		return "", fmt.Errorf("read the agent DaemonSet: %w", err)
	}
	if len(ds.Spec.Template.Spec.Containers) == 0 {
		return "", errors.New("the agent DaemonSet has no container")
	}
	c := ds.Spec.Template.Spec.Containers[0]
	repo := c.Image
	if at := strings.Index(repo, "@"); at >= 0 {
		repo = repo[:at]
	}
	if colon := strings.LastIndex(repo, ":"); colon > strings.LastIndex(repo, "/") {
		repo = repo[:colon]
	}
	image := repo + ":" + version
	if image == c.Image {
		return image, nil
	}
	// A strategic merge patch merges the containers by name: the agent's
	// arguments, environment and mounts stay as the manifest set them. A
	// JSON merge patch would replace the whole list with this one entry.
	patch := map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []map[string]string{{"name": c.Name, "image": image}},
	}}}}
	if err := d.o.API.StrategicMergePatch(ctx, kubeapi.DaemonSetPath(d.o.Namespace, DaemonSetName), patch, nil); err != nil {
		return "", fmt.Errorf("update the agent DaemonSet image: %w", err)
	}
	d.o.Log.Info("agent DaemonSet moved to a new image", "image", image)
	return image, nil
}

// NodeExists reports whether a node of this cluster still exists.
func (d *Driver) NodeExists(ctx context.Context, name string) (bool, error) {
	var n kubeapi.Node
	err := d.o.API.Get(ctx, kubeapi.NodePath(name), &n)
	if kubeapi.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
