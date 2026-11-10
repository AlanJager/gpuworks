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
	// pullSecrets are the agent pod's image pull secrets, for holdImage.
	pullSecrets []kubeapi.LocalRef
	owner       *kubeapi.OwnerReference
	// cards maps device index to card UUID, from the node's own probe.
	cards func(context.Context) (map[int]string, error)
	// idleReturn is how long an unused borrowed card is kept; negative keeps
	// it for good.
	idleReturn time.Duration
	now        func() time.Time
	log        *slog.Logger
	poll       time.Duration
	// releaseWait and borrowRetry bound the waits of cards_borrow.go; zero
	// uses the defaults.
	releaseWait, borrowRetry time.Duration

	// borrowMu serializes borrows. A borrow talks to the API server and the
	// kubelet for up to minutes, so it never runs under mu, which status
	// reporting (holders) and instance bookkeeping take.
	borrowMu sync.Mutex
	// mu guards everything below.
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
	// returning are the hold pods this pool deleted and the kubelet still
	// lists; their cards are not held, whatever the allocations say.
	returning map[string]bool
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
	p.returning = map[string]bool{}
	if p.resource != "" && p.shared == "" {
		held, err := p.listHeld(ctx, p.resource)
		if err != nil {
			return err
		}
		p.setHeld(held)
	}
	pods, err := p.instancePods(ctx)
	if err != nil {
		return err
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

// instancePods lists the instance pods on this node.
func (p *cardPool) instancePods(ctx context.Context) (kubeapi.PodList, error) {
	var pods kubeapi.PodList
	q := kubeapi.ListQuery(LabelRole+"="+RoleInstance+","+LabelNode+"="+p.node, map[string]string{"spec.nodeName": p.node})
	if err := p.api.Get(ctx, kubeapi.PodsPath(p.ns)+q, &pods); err != nil {
		return pods, fmt.Errorf("list instance pods: %w", err)
	}
	return pods, nil
}

func (p *cardPool) use(card, instance string) {
	if p.users[card] == nil {
		p.users[card] = map[string]bool{}
	}
	p.users[card][instance] = true
	delete(p.idleSince, card)
}

// listHeld reads, from the kubelet's allocations, which hold pod of this
// node has which card. It touches no pool state, so it runs with or without
// mu.
func (p *cardPool) listHeld(ctx context.Context, resource string) (map[string]string, error) {
	allocs, err := p.kubelet.List(ctx)
	if err != nil {
		return nil, err
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return nil, err
	}
	held := map[string]string{}
	for _, a := range allocs {
		if a.Namespace != p.ns || !strings.HasPrefix(a.Pod, holdPrefix) || a.Resource != resource {
			continue
		}
		if card := cardOf(a.DeviceID, byIndex); card != "" {
			held[card] = a.Pod
		}
	}
	return held, nil
}

// setHeld records the kubelet's view of the borrowed cards, minus the hold
// pods this pool already deleted (the kubelet lists them until their
// containers are gone). mu is held.
func (p *cardPool) setHeld(seen map[string]string) {
	present := map[string]bool{}
	held := map[string]string{}
	for card, pod := range seen {
		present[pod] = true
		if !p.returning[pod] {
			held[card] = pod
		}
	}
	for pod := range p.returning {
		if !present[pod] {
			delete(p.returning, pod)
		}
	}
	p.held = held
}

const holdPrefix = "gpuworks-hold-"

// acquire makes the cards at indexes the platform's for instance and
// returns their UUIDs. Borrowing from the kubelet is slow, so the pool's
// state is not locked across it: the instance is registered as a user of its
// cards first (which keeps them from being returned meanwhile) and unregistered
// again if the borrow fails.
func (p *cardPool) acquire(ctx context.Context, instance string, indexes []int) ([]string, error) {
	if len(indexes) == 0 {
		return nil, nil
	}
	want, missing, err := p.claim(ctx, instance, indexes)
	if err != nil {
		return nil, err
	}
	if missing {
		if err := p.borrowWithRetry(ctx, want); err != nil {
			p.release(instance)
			return nil, err
		}
	}
	return want, nil
}

// claim registers instance as a user of the cards at indexes and reports
// their UUIDs and whether the platform still has to borrow some of them.
func (p *cardPool) claim(ctx context.Context, instance string, indexes []int) ([]string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return nil, false, err
	}
	if p.shared != "" {
		return nil, false, errors.New(p.shared)
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return nil, false, err
	}
	want := make([]string, 0, len(indexes))
	for _, i := range indexes {
		uuid := byIndex[i]
		if uuid == "" {
			if p.resource == "" {
				continue // nothing to borrow; the card is used by index
			}
			return nil, false, fmt.Errorf("card %d has no UUID on this node", i)
		}
		want = append(want, uuid)
	}
	missing := false
	for _, card := range want {
		if p.resource != "" && p.held[card] == "" {
			missing = true
		}
		p.use(card, instance)
	}
	return want, missing, nil
}

// release says instance no longer uses its cards.
func (p *cardPool) release(instance string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releaseLocked(instance)
}

func (p *cardPool) releaseLocked(instance string) {
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
	if !p.loaded || p.idleReturn < 0 {
		p.mu.Unlock()
		return
	}
	var pods []string
	for card, since := range p.idleSince {
		if len(p.users[card]) > 0 || p.now().Sub(since) < p.idleReturn {
			continue
		}
		if pod := p.held[card]; pod != "" {
			p.log.Info("returning an idle card to Kubernetes", "card", card, "pod", pod)
			pods = append(pods, pod)
			p.returning[pod] = true
		}
		delete(p.held, card)
		delete(p.idleSince, card)
	}
	p.mu.Unlock()
	p.deletePods(ctx, pods)
}

// keepAll borrows every free card for good (a cluster that keeps cards).
func (p *cardPool) keepAll(ctx context.Context) error {
	p.borrowMu.Lock()
	defer p.borrowMu.Unlock()
	p.mu.Lock()
	err := p.load(ctx)
	skip := err != nil || p.resource == "" || p.shared != ""
	p.mu.Unlock()
	if skip {
		return err
	}
	if _, _, err := p.takeAllFree(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
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
