//go:build linux && dqlite

package app

import (
	"net"
	"os"
	"strings"
)

const (
	// EntryReasonAddress means the address is unusable or already present.
	EntryReasonAddress = "address"
	// EntryReasonInterface means the named interface is missing or down.
	EntryReasonInterface = "interface"
	// EntryReasonPrivilege means this process cannot add the address.
	EntryReasonPrivilege = "privilege"
	// EntryReasonNetwork means the network cannot move the address.
	EntryReasonNetwork = "network"
)

// EntryPreflightError is the result of one cluster-entry address check.
// Reason is address, interface, privilege, or network.
type EntryPreflightError struct {
	Reason string
	detail string
}

func (e *EntryPreflightError) Error() string {
	if e == nil {
		return ""
	}
	if e.detail != "" {
		return e.detail
	}
	switch e.Reason {
	case EntryReasonAddress:
		return "entry address is already present on a local interface"
	case EntryReasonInterface:
		return "entry interface is missing or down"
	case EntryReasonPrivilege:
		return "entry address needs network privilege and the server keeps its current user"
	case EntryReasonNetwork:
		return "this network does not allow the entry address to move"
	default:
		return "entry preflight failed"
	}
}

type entryPreflightConfig struct {
	address string
	iface   string
	drift   bool
	observe func() (entryFacts, error)
}

type entryFacts struct {
	interfaces []entryIface
	privileged bool
}

type entryIface struct {
	name  string
	up    bool
	addrs []string
}

// SetEntryPreflight installs the cluster-entry address check. An empty
// address leaves the process on its current user. observe nil reads the
// local interfaces. The check reports privilege and does not change the
// process user.
func (a *App) SetEntryPreflight(address, iface string, drift bool, observe func() (entryFacts, error)) {
	if a == nil {
		return
	}
	a.entryPreflightMu.Lock()
	defer a.entryPreflightMu.Unlock()
	if strings.TrimSpace(address) == "" && observe == nil {
		a.entryPreflight = nil
		return
	}
	a.entryPreflight = &entryPreflightConfig{address: address, iface: iface, drift: drift, observe: observe}
}

// prepareEntry runs before the listener. No configured address is success.
// A failed check returns before the process listens, and the user is unchanged.
func (a *App) prepareEntry() error {
	if a == nil {
		return nil
	}
	a.entryPreflightMu.Lock()
	cfg := a.entryPreflight
	a.entryPreflightMu.Unlock()
	if cfg == nil || strings.TrimSpace(cfg.address) == "" {
		return nil
	}
	observe := cfg.observe
	if observe == nil {
		observe = localEntryFacts
	}
	facts, err := observe()
	if err != nil {
		return err
	}
	return checkEntry(*cfg, facts)
}

func checkEntry(cfg entryPreflightConfig, facts entryFacts) error {
	address := strings.TrimSpace(cfg.address)
	if address == "" {
		return nil
	}
	ip := net.ParseIP(address)
	if ip == nil {
		return &EntryPreflightError{Reason: EntryReasonAddress, detail: "entry address is not an IP address"}
	}
	var found *entryIface
	name := strings.TrimSpace(cfg.iface)
	for i := range facts.interfaces {
		if facts.interfaces[i].name == name {
			found = &facts.interfaces[i]
			break
		}
	}
	if found == nil || !found.up {
		return &EntryPreflightError{Reason: EntryReasonInterface}
	}
	for _, iface := range facts.interfaces {
		for _, raw := range iface.addrs {
			if entryIPEqual(ip, raw) {
				return &EntryPreflightError{Reason: EntryReasonAddress}
			}
		}
	}
	if !facts.privileged {
		return &EntryPreflightError{Reason: EntryReasonPrivilege}
	}
	if !cfg.drift {
		return &EntryPreflightError{Reason: EntryReasonNetwork}
	}
	return nil
}

func entryIPEqual(ip net.IP, raw string) bool {
	raw = strings.TrimSpace(raw)
	if parsed := net.ParseIP(raw); parsed != nil {
		return parsed.Equal(ip)
	}
	parsed, _, err := net.ParseCIDR(raw)
	return err == nil && parsed.Equal(ip)
}

func localEntryFacts() (entryFacts, error) {
	list, err := net.Interfaces()
	if err != nil {
		return entryFacts{}, err
	}
	facts := entryFacts{privileged: os.Geteuid() == 0}
	for _, iface := range list {
		item := entryIface{name: iface.Name, up: iface.Flags&net.FlagUp != 0}
		addrs, err := iface.Addrs()
		if err != nil {
			return entryFacts{}, err
		}
		for _, addr := range addrs {
			item.addrs = append(item.addrs, addr.String())
		}
		facts.interfaces = append(facts.interfaces, item)
	}
	return facts, nil
}
