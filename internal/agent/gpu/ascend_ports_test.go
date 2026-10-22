package gpu

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// fakeHCCN answers like hccn_tool on the 910B4 hosts: every answer exits 0,
// a card the host does not own never answers (the caller's time limit ends
// it), and a card index that does not exist says so.
type fakeHCCN struct {
	mu       sync.Mutex
	cards    map[string]map[string]string // card index -> question -> answer
	hang     map[string]bool
	calls    []string
	inFlight map[string]int
	overlap  bool // two calls on one card ran at once
}

func (f *fakeHCCN) run(_ context.Context, args ...string) (string, error) {
	if len(args) != 4 || args[0] != "-i" || args[3] != "-g" {
		return "Get invalid major cmd.\nEnter [ hccn_tool -h ] for more details.\n", nil
	}
	card, what := args[1], args[2]
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.inFlight == nil {
		f.inFlight = map[string]int{}
	}
	f.inFlight[card]++
	if f.inFlight[card] > 1 {
		f.overlap = true
	}
	f.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight[card]--
	if f.hang[card] {
		return "", context.DeadlineExceeded
	}
	answers, ok := f.cards[card]
	if !ok {
		return "Get invalid device id.\n", nil
	}
	return answers[what], nil
}

func hccnCard(ip, link, speed string) map[string]string {
	return map[string]string{
		"-ip":    "ipaddr:" + ip + "\nnetmask:255.255.0.0\n",
		"-link":  "link status: " + link + "\n",
		"-speed": "Speed: " + speed + "\n",
	}
}

func ascendPortProber(hccn *fakeHCCN, info string) *AscendProber {
	return &AscendProber{
		infoTTL: time.Hour,
		runner:  func(context.Context, ...string) (string, error) { return info, nil },
		hccn:    hccn.run,
	}
}

func TestAscendAcceleratorPortsReadEachListedCard(t *testing.T) {
	hccn := &fakeHCCN{cards: map[string]map[string]string{
		"2": hccnCard("172.20.9.58", "UP", "200000 Mb/s"),
		"4": hccnCard("172.20.9.60", "DOWN", "Unknown!"),
	}}
	p := ascendPortProber(hccn, ascendInfo26Sample)
	got := p.AcceleratorPorts(t.Context())
	want := []proto.AcceleratorPort{
		{Name: "npu2", Index: 2, IP: "172.20.9.58", Netmask: "255.255.0.0", State: "UP", SpeedGbps: 200},
		{Name: "npu4", Index: 4, IP: "172.20.9.60", Netmask: "255.255.0.0", State: "DOWN"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %+v, want %+v", got, want)
	}
	if hccn.overlap {
		t.Fatal("two hccn_tool calls ran on one card at once")
	}
	// Only the cards npu-smi lists are asked: the others may belong to a VM.
	for _, call := range hccn.calls {
		if !strings.HasPrefix(call, "-i 2 ") && !strings.HasPrefix(call, "-i 4 ") {
			t.Fatalf("asked a card npu-smi did not list: %q", call)
		}
	}
	if len(hccn.calls) != 6 {
		t.Fatalf("calls = %v, want three per card", hccn.calls)
	}
}

func TestAscendAcceleratorPortsAreCached(t *testing.T) {
	hccn := &fakeHCCN{cards: map[string]map[string]string{"2": hccnCard("172.20.9.58", "UP", "200000 Mb/s"), "4": hccnCard("172.20.9.60", "UP", "200000 Mb/s")}}
	p := ascendPortProber(hccn, ascendInfo26Sample)
	first := p.AcceleratorPorts(t.Context())
	first[0].IP = "changed by the caller"
	calls := len(hccn.calls)
	if second := p.AcceleratorPorts(t.Context()); len(hccn.calls) != calls || second[0].IP != "172.20.9.58" {
		t.Fatalf("second read: calls %d -> %d, ports %+v", calls, len(hccn.calls), second)
	}
	p.portsTTL = time.Nanosecond
	time.Sleep(time.Millisecond)
	p.AcceleratorPorts(t.Context())
	if len(hccn.calls) == calls {
		t.Fatal("ports were not read again after the cache expired")
	}
}

func TestAscendAcceleratorPortsLeaveOutCardsThatDoNotAnswer(t *testing.T) {
	// Card 2 hangs until the time limit, as a card passed through to a VM
	// does; card 4 is an index hccn_tool does not know.
	hccn := &fakeHCCN{cards: map[string]map[string]string{}, hang: map[string]bool{"2": true}}
	p := ascendPortProber(hccn, ascendInfo26Sample)
	if got := p.AcceleratorPorts(t.Context()); len(got) != 0 {
		t.Fatalf("ports = %#v, want none", got)
	}

	// A card with a link state but no address is still a port.
	hccn = &fakeHCCN{cards: map[string]map[string]string{"4": {"-ip": "", "-link": "link status: DOWN\n", "-speed": "Speed: Unknown!\n"}}, hang: map[string]bool{"2": true}}
	p = ascendPortProber(hccn, ascendInfo26Sample)
	want := []proto.AcceleratorPort{{Name: "npu4", Index: 4, State: "DOWN"}}
	if got := p.AcceleratorPorts(t.Context()); !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %+v, want %+v", got, want)
	}
}

func TestAscendAcceleratorPortsNeedAWorkingProbe(t *testing.T) {
	hccn := &fakeHCCN{cards: map[string]map[string]string{"2": hccnCard("172.20.9.58", "UP", "200000 Mb/s")}}
	p := &AscendProber{
		infoTTL: time.Hour,
		runner:  func(context.Context, ...string) (string, error) { return "", errors.New("driver busy") },
		hccn:    hccn.run,
	}
	if got := p.AcceleratorPorts(t.Context()); got != nil || len(hccn.calls) != 0 {
		t.Fatalf("ports = %+v after %d hccn_tool calls, want none", got, len(hccn.calls))
	}
}

func TestParseHCCNAnswers(t *testing.T) {
	if ip, mask := parseHCCNAddress("ipaddr:172.20.9.59\nnetmask:255.255.0.0\n"); ip != "172.20.9.59" || mask != "255.255.0.0" {
		t.Fatalf("address = %q %q", ip, mask)
	}
	for _, out := range []string{"", "Get invalid device id.\n", "Get gateway failed, because no gateway was preset there!\n"} {
		if ip, mask := parseHCCNAddress(out); ip != "" || mask != "" {
			t.Fatalf("address of %q = %q %q", out, ip, mask)
		}
		if got := parseHCCNLink(out); got != "" {
			t.Fatalf("link of %q = %q", out, got)
		}
		if got := parseHCCNSpeed(out); got != 0 {
			t.Fatalf("speed of %q = %v", out, got)
		}
	}
	if got := parseHCCNLink("link status: DOWN\n"); got != "DOWN" {
		t.Fatalf("link = %q", got)
	}
	if got := parseHCCNSpeed("Speed: 200000 Mb/s\n"); got != 200 {
		t.Fatalf("speed = %v", got)
	}
	if got := parseHCCNSpeed("Speed: Unknown!\n"); got != 0 {
		t.Fatalf("unknown speed = %v", got)
	}
}

// fakePing answers "hccn_tool -i N -ping -g address IP" the way the 910B4
// hosts do: a reachable address prints three answers at once after about a
// second, an unreachable one prints nothing and never returns, and a card
// asked twice at once answers "Command execute failed!".
type fakePing struct {
	mu        sync.Mutex
	reachable map[string]bool // "card->ip"
	delay     time.Duration
	inFlight  map[string]int
	calls     []string
	killed    int
}

func (f *fakePing) run(ctx context.Context, args ...string) (string, error) {
	if len(args) != 6 || args[0] != "-i" || args[2] != "-ping" || args[3] != "-g" || args[4] != "address" {
		return hccnCard("172.20.9.1", "UP", "200000 Mb/s")[args[2]], nil
	}
	card, ip := args[1], args[5]
	f.mu.Lock()
	f.calls = append(f.calls, card+"->"+ip)
	if f.inFlight == nil {
		f.inFlight = map[string]int{}
	}
	f.inFlight[card]++
	busy := f.inFlight[card] > 1
	ok := f.reachable[card+"->"+ip]
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight[card]--
		f.mu.Unlock()
	}()
	if busy {
		return "Command execute failed!\n", nil
	}
	if !ok {
		<-ctx.Done()
		f.mu.Lock()
		f.killed++
		f.mu.Unlock()
		return "", ctx.Err()
	}
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "device " + card + " PING " + ip + "\nrecv seq=0,time=0.359000ms\nrecv seq=1,time=0.238000ms\nrecv seq=2,time=0.224000ms\n3 packets transmitted, 3 received, 0.00% packet loss\n", nil
}

func TestAscendPingAcceleratorPort(t *testing.T) {
	ping := &fakePing{reachable: map[string]bool{"3->172.20.9.82": true}, delay: 5 * time.Millisecond}
	p := &AscendProber{hccn: ping.run}
	rtt, err := p.PingAcceleratorPort(t.Context(), "npu3", "172.20.9.82", time.Second)
	if err != nil || rtt < 0.27 || rtt > 0.28 {
		t.Fatalf("reachable ping = %v, %v; want the mean of the three answers", rtt, err)
	}
	// No answer: the tool hangs, the limit ends it, and the card can ping
	// again at once.
	start := time.Now()
	if _, err := p.PingAcceleratorPort(t.Context(), "npu3", "172.20.9.83", 30*time.Millisecond); !errors.Is(err, ErrAcceleratorPingTimeout) {
		t.Fatalf("unreachable ping err = %v", err)
	}
	if took := time.Since(start); took > 500*time.Millisecond || ping.killed != 1 {
		t.Fatalf("unreachable ping took %v, killed %d", took, ping.killed)
	}
	if _, err := p.PingAcceleratorPort(t.Context(), "npu3", "172.20.9.82", time.Second); err != nil {
		t.Fatalf("ping after a timed-out one: %v", err)
	}
	// Anything that is not an IPv4 address never reaches the tool.
	calls := len(ping.calls)
	for _, target := range []string{"", "node-2", "172.20.9.82; rm -rf /", "fd00::1", "address"} {
		if _, err := p.PingAcceleratorPort(t.Context(), "npu3", target, time.Second); err == nil || errors.Is(err, ErrAcceleratorPingTimeout) {
			t.Fatalf("target %q: err = %v", target, err)
		}
	}
	if len(ping.calls) != calls {
		t.Fatalf("an invalid target reached hccn_tool: %v", ping.calls[calls:])
	}
	// The caller's cancellation ends a hung ping too.
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if _, err := p.PingAcceleratorPort(ctx, "npu3", "172.20.9.83", time.Minute); !errors.Is(err, ErrAcceleratorPingTimeout) {
		t.Fatalf("cancelled ping err = %v", err)
	}
}

// Two pings on one card never overlap, whoever starts them; different cards
// do run side by side.
func TestAscendPingsOnOneCardRunInTurn(t *testing.T) {
	ping := &fakePing{reachable: map[string]bool{"3->172.20.9.82": true, "4->172.20.9.83": true}, delay: 40 * time.Millisecond}
	p := &AscendProber{hccn: ping.run}
	var wg sync.WaitGroup
	errs := make([]error, 6)
	start := time.Now()
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			card, target := "npu3", "172.20.9.82"
			if i%2 == 1 {
				card, target = "npu4", "172.20.9.83"
			}
			_, errs[i] = p.PingAcceleratorPort(t.Context(), card, target, time.Second)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ping %d: %v (a card was asked twice at once)", i, err)
		}
	}
	// Three pings per card in turn, the two cards in parallel: about three
	// delays, not six.
	if took := time.Since(start); took < 110*time.Millisecond || took > 230*time.Millisecond {
		t.Fatalf("six pings took %v, want about 120ms", took)
	}
}

func TestAscendPingReportsTheToolsOwnFailure(t *testing.T) {
	answers := map[string]string{
		"failed":  "Command execute failed!\n",
		"invalid": "Get invalid device id.\n",
		"lost":    "device 3 PING 172.20.9.82\n3 packets transmitted, 0 received, 100.00% packet loss\n",
		"silent":  "",
	}
	for name, out := range answers {
		p := &AscendProber{hccn: func(context.Context, ...string) (string, error) { return out, nil }}
		_, err := p.PingAcceleratorPort(t.Context(), "npu3", "172.20.9.82", time.Second)
		if err == nil || errors.Is(err, ErrAcceleratorPingTimeout) {
			t.Fatalf("%s: err = %v, want the tool's failure", name, err)
		}
		if name == "failed" && err.Error() != "Command execute failed!" {
			t.Fatalf("failed: err = %q", err)
		}
	}
	// One answer out of three is still an answer.
	p := &AscendProber{hccn: func(context.Context, ...string) (string, error) {
		return "device 3 PING 172.20.9.82\nrecv seq=2,time=0.500000ms\n3 packets transmitted, 1 received, 66.67% packet loss\n", nil
	}}
	if rtt, err := p.PingAcceleratorPort(t.Context(), "npu3", "172.20.9.82", time.Second); err != nil || rtt != 0.5 {
		t.Fatalf("partial loss = %v, %v", rtt, err)
	}
}

// A card is checked against the last inventory, without reading a new one:
// a refresh waits for the cards' locks, which the pings of a test hold.
func TestAscendPingKnowsItsCardsFromTheLastInventory(t *testing.T) {
	ping := &fakePing{reachable: map[string]bool{"2->172.20.9.82": true, "7->172.20.9.82": true}}
	p := &AscendProber{infoTTL: time.Hour, hccn: ping.run, runner: func(context.Context, ...string) (string, error) { return ascendInfo26Sample, nil }}
	// Names that are not this vendor's cards never reach the tool.
	for _, card := range []string{"", "npu", "mlx5_0:1", "npu03", "gpu2"} {
		if _, err := p.PingAcceleratorPort(t.Context(), card, "172.20.9.82", time.Second); !errors.Is(err, ErrAcceleratorNoCard) {
			t.Fatalf("card %q: err = %v", card, err)
		}
	}
	if len(ping.calls) != 0 {
		t.Fatalf("calls = %v", ping.calls)
	}
	// Before any inventory the tool is left to say; card 7 answers here.
	if _, err := p.PingAcceleratorPort(t.Context(), "npu7", "172.20.9.82", time.Second); err != nil {
		t.Fatalf("before an inventory: %v", err)
	}
	// The inventory lists cards 2 and 4 (the sample npu-smi table).
	if ports := p.AcceleratorPorts(t.Context()); len(ports) != 2 {
		t.Fatalf("ports = %+v", ports)
	}
	calls := len(ping.calls)
	if _, err := p.PingAcceleratorPort(t.Context(), "npu7", "172.20.9.82", time.Second); !errors.Is(err, ErrAcceleratorNoCard) {
		t.Fatalf("a card the inventory lacks: err = %v", err)
	}
	if len(ping.calls) != calls {
		t.Fatal("a card the node does not own reached the tool")
	}
	if _, err := p.PingAcceleratorPort(t.Context(), "npu2", "172.20.9.82", time.Second); err != nil {
		t.Fatalf("an owned card: %v", err)
	}
	if len(ping.calls) != calls+1 {
		t.Fatalf("calls after an owned card's ping = %d, want %d: a ping must not re-read the inventory", len(ping.calls), calls+1)
	}
}
