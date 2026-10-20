package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/podresources"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Borrowing cards from the kubelet.
//
// Every card on a node has exactly one allocator. Where a device plugin
// advertises the node's cards, that is the kubelet; the platform takes a card
// from it by running a hold pod that requests that one card, and gives it
// back by deleting the pod. A device plugin cannot be told which card to
// give, so borrowing takes every free card at once — then there is no choice
// left to make — finds out from the kubelet which hold pod got which card,
// and returns the ones it does not need. Instance pods request no device
// resource; they get their cards the way docker containers do, through the
// runtime class. A card stays borrowed while an instance uses it and for
// IdleReturn after the last one stopped, so a redeploy finds it again.
//
// Where no device plugin advertises the cards, the kubelet allocates none of
// them, and the platform simply uses them.

// allocationLister is the kubelet's PodResources List (podresources.Client).
type allocationLister interface {
	List(ctx context.Context) ([]podresources.Allocation, error)
}

type cardPool struct {
	api       *kubeapi.Client
	kubelet   allocationLister
	ns, node  string
	holdImage string
	holdCmd   []string
	owner     *kubeapi.OwnerReference
	// cards maps device index to card UUID, from the node's own probe.
	cards func(context.Context) (map[int]string, error)
	// idleReturn is how long an unused borrowed card is kept; negative keeps
	// it for good.
	idleReturn time.Duration
	now        func() time.Time
	log        *slog.Logger
	poll       time.Duration

	mu       sync.Mutex
	resource string // the device plugin resource; "" = none advertised
	// shared says why the node's cards cannot be borrowed: the device
	// plugin hands out parts of cards (time-slicing, MIG, HAMi, vNPU), so a
	// unit Kubernetes allocates is not a card the platform could own. The
	// cards are then reported as used outside the platform. "" = whole cards.
	shared    string
	held      map[string]string // card UUID -> hold pod name
	users     map[string]map[string]bool
	idleSince map[string]time.Time
	loaded    bool
}

var deviceIndexSuffix = regexp.MustCompile(`-(\d+)$`)

// cardOf maps a device plugin's device ID to a card UUID: NVIDIA's are the
// UUIDs themselves, others end in the device index.
func cardOf(id string, byIndex map[int]string) string {
	for _, uuid := range byIndex {
		if uuid == id {
			return uuid
		}
	}
	if m := deviceIndexSuffix.FindStringSubmatch(id); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return byIndex[n]
		}
	}
	return ""
}

func randomSuffix() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// classify reads which device resource the node advertises, and whether it
// advertises whole cards. It runs on every load, so a device plugin installed
// or reconfigured after the agent started is noticed.
func (p *cardPool) classify(ctx context.Context) error {
	var node kubeapi.Node
	if err := p.api.Get(ctx, kubeapi.NodePath(p.node)+"?resourceVersion=0", &node); err != nil {
		return fmt.Errorf("read node %s: %w", p.node, err)
	}
	capacity := node.Status.Capacity
	if len(capacity) == 0 {
		capacity = node.Status.Allocatable
	}
	var whole, parts []string
	for name, qty := range capacity {
		if qty == "0" {
			continue
		}
		switch kind, _ := accelerator.ClassifyKubeResource(name); kind {
		case accelerator.KubeResourceCard:
			whole = append(whole, name)
		case accelerator.KubeResourceShared:
			parts = append(parts, name)
		}
	}
	sort.Strings(whole)
	sort.Strings(parts)
	p.resource, p.shared = "", ""
	if len(whole) > 0 {
		p.resource = whole[0]
	}
	if len(parts) > 0 {
		p.shared = "Kubernetes hands out parts of this node's cards (" + strings.Join(parts, ", ") + "); the platform borrows whole cards only"
		return nil
	}
	if p.resource != "" {
		byIndex, err := p.cards(ctx)
		if err != nil {
			return err
		}
		if n, _ := strconv.Atoi(capacity[p.resource]); len(byIndex) > 0 && n > len(byIndex) {
			p.shared = fmt.Sprintf("Kubernetes advertises %d %s on %d cards (time-slicing or MPS shares them); the platform borrows whole cards only", n, p.resource, len(byIndex))
		}
	}
	return nil
}

// load classifies the node's cards and, the first time, rebuilds the pool
// from the hold pods and instance pods already on the node, so a restarted
// agent picks up where it left off.
func (p *cardPool) load(ctx context.Context) error {
	if err := p.classify(ctx); err != nil {
		return err
	}
	if p.loaded {
		return nil
	}
	p.held, p.users, p.idleSince = map[string]string{}, map[string]map[string]bool{}, map[string]time.Time{}
	if p.resource != "" && p.shared == "" {
		if err := p.refreshHeld(ctx); err != nil {
			return err
		}
	}
	var pods kubeapi.PodList
	q := kubeapi.ListQuery(LabelRole+"="+RoleInstance+","+LabelNode+"="+p.node, map[string]string{"spec.nodeName": p.node})
	if err := p.api.Get(ctx, kubeapi.PodsPath(p.ns)+q, &pods); err != nil {
		return fmt.Errorf("list instance pods: %w", err)
	}
	for _, pod := range pods.Items {
		for _, card := range strings.Split(pod.Metadata.Annotations[AnnotationCards], ",") {
			if card != "" {
				p.use(card, pod.Metadata.Name)
			}
		}
	}
	for card := range p.held {
		if len(p.users[card]) == 0 {
			p.idleSince[card] = p.now()
		}
	}
	p.loaded = true
	return nil
}

func (p *cardPool) use(card, instance string) {
	if p.users[card] == nil {
		p.users[card] = map[string]bool{}
	}
	p.users[card][instance] = true
	delete(p.idleSince, card)
}

// refreshHeld rebuilds held from the kubelet's allocations to this node's
// hold pods.
func (p *cardPool) refreshHeld(ctx context.Context) error {
	allocs, err := p.kubelet.List(ctx)
	if err != nil {
		return err
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return err
	}
	held := map[string]string{}
	for _, a := range allocs {
		if a.Namespace != p.ns || !strings.HasPrefix(a.Pod, holdPrefix) || a.Resource != p.resource {
			continue
		}
		if card := cardOf(a.DeviceID, byIndex); card != "" {
			held[card] = a.Pod
		}
	}
	p.held = held
	return nil
}

const holdPrefix = "gpuworks-hold-"

// acquire makes the cards at indexes the platform's for instance and
// returns their UUIDs.
func (p *cardPool) acquire(ctx context.Context, instance string, indexes []int) ([]string, error) {
	if len(indexes) == 0 {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return nil, err
	}
	if p.shared != "" {
		return nil, errors.New(p.shared)
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return nil, err
	}
	want := make([]string, 0, len(indexes))
	for _, i := range indexes {
		uuid := byIndex[i]
		if uuid == "" {
			if p.resource == "" {
				continue // nothing to borrow; the card is used by index
			}
			return nil, fmt.Errorf("card %d has no UUID on this node", i)
		}
		want = append(want, uuid)
	}
	if p.resource != "" {
		if err := p.borrow(ctx, want); err != nil {
			return nil, err
		}
	}
	for _, card := range want {
		p.use(card, instance)
	}
	return want, nil
}

// borrow takes every free card, keeps the ones in want and returns the rest.
func (p *cardPool) borrow(ctx context.Context, want []string) error {
	missing := 0
	for _, card := range want {
		if p.held[card] == "" {
			missing++
		}
	}
	if missing == 0 {
		return nil
	}
	if err := p.checkHolders(ctx, want); err != nil {
		return err
	}
	_, refused, err := p.takeAllFree(ctx)
	if err != nil {
		return err
	}
	var still []string
	for _, card := range want {
		if p.held[card] == "" {
			still = append(still, card)
		}
	}
	keep := map[string]bool{}
	for _, card := range want {
		keep[card] = true
	}
	p.returnUnused(ctx, keep)
	if len(still) > 0 {
		why := "another pod took them first"
		if refused != "" {
			why = "the kubelet refused the hold pods: " + refused
		}
		return fmt.Errorf("Kubernetes did not give this node's cards %s to the platform: %s", strings.Join(still, ","), why)
	}
	return nil
}

// checkHolders fails fast when a wanted card is allocated to a pod that is
// not the platform's.
func (p *cardPool) checkHolders(ctx context.Context, want []string) error {
	holders, err := p.externalHolders(ctx)
	if err != nil {
		return err
	}
	for _, card := range want {
		if h := holders[card]; len(h) > 0 {
			return fmt.Errorf("card %s is allocated to %s by Kubernetes", card, h[0].Name)
		}
	}
	return nil
}

// takeAllFree creates one hold pod per free card and waits for the kubelet
// to admit them; it returns how many it got and, when some were refused,
// the kubelet's reason (node pressure, a failed allocation).
func (p *cardPool) takeAllFree(ctx context.Context) (int, string, error) {
	free, err := p.freeCount(ctx)
	if err != nil || free <= 0 {
		return 0, "", err
	}
	names := make([]string, 0, free)
	for i := 0; i < free; i++ {
		name := holdPrefix + randomSuffix()
		if err := p.api.Create(ctx, kubeapi.PodsPath(p.ns), p.holdPod(name), nil); err != nil {
			p.deletePods(ctx, names)
			return 0, "", fmt.Errorf("create hold pod: %w", err)
		}
		names = append(names, name)
	}
	admitted, refused := p.waitAdmitted(ctx, names)
	var failed []string
	for _, n := range names {
		if !admitted[n] {
			failed = append(failed, n)
		}
	}
	p.deletePods(ctx, failed)
	if err := p.refreshHeld(ctx); err != nil {
		return 0, "", err
	}
	return len(names) - len(failed), refused, nil
}

// freeCount is how many of the advertised cards no pod holds.
func (p *cardPool) freeCount(ctx context.Context) (int, error) {
	var node kubeapi.Node
	if err := p.api.Get(ctx, kubeapi.NodePath(p.node), &node); err != nil {
		return 0, err
	}
	total, _ := strconv.Atoi(node.Status.Allocatable[p.resource])
	allocs, err := p.kubelet.List(ctx)
	if err != nil {
		return 0, err
	}
	used := map[string]bool{}
	for _, a := range allocs {
		if a.Resource == p.resource {
			used[a.DeviceID] = true
		}
	}
	return total - len(used), nil
}

func (p *cardPool) holdPod(name string) kubeapi.Pod {
	grace := int64(0)
	pod := kubeapi.Pod{
		APIVersion: "v1", Kind: "Pod",
		Metadata: kubeapi.ObjectMeta{Name: name, Namespace: p.ns,
			Labels: map[string]string{LabelManagedBy: "gpuworks", LabelRole: RoleHold, LabelNode: p.node}},
		Spec: kubeapi.PodSpec{
			NodeName: p.node, RestartPolicy: "Always", TerminationGracePeriodSeconds: &grace,
			PriorityClassName: "gpuworks-inference", Tolerations: []kubeapi.Toleration{{Operator: "Exists"}},
			Containers: []kubeapi.Container{{
				Name: "hold", Image: p.holdImage, ImagePullPolicy: "IfNotPresent", Command: p.holdCmd,
				Resources: &kubeapi.Resources{
					Requests: map[string]string{p.resource: "1", "cpu": "10m", "memory": "16Mi"},
					Limits:   map[string]string{p.resource: "1"},
				},
			}},
		},
	}
	if p.owner != nil {
		pod.Metadata.OwnerReferences = []kubeapi.OwnerReference{*p.owner}
	}
	return pod
}

// waitAdmitted waits until each hold pod runs or the kubelet refused it, and
// returns the first refusal's reason.
func (p *cardPool) waitAdmitted(ctx context.Context, names []string) (map[string]bool, string) {
	out, refused := map[string]bool{}, ""
	pending := map[string]bool{}
	for _, n := range names {
		pending[n] = true
	}
	deadline := p.now().Add(60 * time.Second)
	for len(pending) > 0 && p.now().Before(deadline) && ctx.Err() == nil {
		for n := range pending {
			var pod kubeapi.Pod
			err := p.api.Get(ctx, kubeapi.PodPath(p.ns, n), &pod)
			switch {
			case err != nil && kubeapi.IsNotFound(err):
				delete(pending, n)
			case err != nil:
			case pod.Status.Phase == "Running":
				out[n] = true
				delete(pending, n)
			case pod.Status.Phase == "Failed":
				// A failed device allocation is the race borrow already
				// describes; anything else (eviction under node pressure)
				// is the reason worth reporting.
				if refused == "" && pod.Status.Reason != "UnexpectedAdmissionError" {
					refused = strings.TrimSpace(pod.Status.Reason + ": " + pod.Status.Message)
				}
				delete(pending, n)
			}
		}
		if len(pending) > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(p.poll):
			}
		}
	}
	return out, refused
}

func (p *cardPool) deletePods(ctx context.Context, names []string) {
	zero := int64(0)
	for _, n := range names {
		if err := p.api.Delete(ctx, kubeapi.PodPath(p.ns, n), &zero); err != nil {
			p.log.Warn("delete hold pod failed", "pod", n, "err", err)
		}
	}
}

// returnUnused deletes the hold pods of borrowed cards nobody uses, keeping
// the ones in keep.
func (p *cardPool) returnUnused(ctx context.Context, keep map[string]bool) {
	for card, pod := range p.held {
		if keep[card] || len(p.users[card]) > 0 {
			continue
		}
		if _, idle := p.idleSince[card]; idle {
			continue // borrowed earlier and still in its grace period
		}
		p.deletePods(ctx, []string{pod})
		delete(p.held, card)
	}
}

// release says instance no longer uses its cards.
func (p *cardPool) release(instance string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for card, users := range p.users {
		if users[instance] {
			delete(users, instance)
			if len(users) == 0 {
				delete(p.users, card)
				if p.held[card] != "" {
					p.idleSince[card] = p.now()
				}
			}
		}
	}
}

// sweep returns the cards that have been idle longer than idleReturn.
func (p *cardPool) sweep(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loaded || p.idleReturn < 0 {
		return
	}
	for card, since := range p.idleSince {
		if len(p.users[card]) > 0 || p.now().Sub(since) < p.idleReturn {
			continue
		}
		if pod := p.held[card]; pod != "" {
			p.log.Info("returning an idle card to Kubernetes", "card", card, "pod", pod)
			p.deletePods(ctx, []string{pod})
		}
		delete(p.held, card)
		delete(p.idleSince, card)
	}
}

// keepAll borrows every free card for good (a cluster that keeps cards).
func (p *cardPool) keepAll(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil || p.resource == "" || p.shared != "" {
		return err
	}
	if _, _, err := p.takeAllFree(ctx); err != nil {
		return err
	}
	for card := range p.held {
		if len(p.users[card]) == 0 {
			p.idleSince[card] = p.now()
		}
	}
	return nil
}

// holders answers the agent's question "who outside the platform uses
// which card": the pods the kubelet gave a card to, or, when the node's
// cards are shared out in parts, every card, since any of them may be.
func (p *cardPool) holders(ctx context.Context) (map[string][]proto.CardProcess, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return nil, err
	}
	if p.shared == "" {
		return p.externalHolders(ctx)
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]proto.CardProcess{}
	for _, uuid := range byIndex {
		out[uuid] = []proto.CardProcess{{Name: p.shared}}
	}
	return out, nil
}

// externalHolders maps card UUID to the pods outside the platform the
// kubelet gave it to, shaped as the foreign processes a card reports.
func (p *cardPool) externalHolders(ctx context.Context) (map[string][]proto.CardProcess, error) {
	allocs, err := p.kubelet.List(ctx)
	if err != nil {
		return nil, err
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]proto.CardProcess{}
	for _, a := range allocs {
		if kind, _ := accelerator.ClassifyKubeResource(a.Resource); kind == accelerator.KubeResourceOther || (a.Namespace == p.ns && strings.HasPrefix(a.Pod, holdPrefix)) {
			continue
		}
		if card := cardOf(a.DeviceID, byIndex); card != "" {
			out[card] = append(out[card], proto.CardProcess{Name: "Pod " + a.Namespace + "/" + a.Pod})
		}
	}
	return out, nil
}
