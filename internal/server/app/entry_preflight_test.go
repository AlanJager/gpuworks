//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestEntryPreflightReportsAddressInterfacePrivilegeAndNetwork(t *testing.T) {
	uid := os.Geteuid()
	facts, err := localEntryFacts()
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != uid {
		t.Fatalf("reading local interfaces changed uid from %d to %d", uid, os.Geteuid())
	}
	if facts.privileged != (uid == 0) {
		t.Fatalf("privilege fact = %t with uid %d", facts.privileged, uid)
	}
	t.Log("reading local interfaces reports the current user and leaves that user in place")

	member := &App{ha: &memberRuntime{}}
	var observed int
	member.SetEntryPreflight("", "eth0", false, func() (entryFacts, error) {
		observed++
		return entryFacts{privileged: false}, nil
	})
	if err := member.prepareEntry(); err != nil || observed != 0 {
		t.Fatalf("empty address err=%v observed=%d", err, observed)
	}
	if os.Geteuid() != uid {
		t.Fatalf("empty address changed uid from %d to %d", uid, os.Geteuid())
	}
	t.Log("a server with no entry address starts without network privilege")

	up := []entryIface{{name: "eth0", up: true, addrs: []string{"192.0.2.1/24"}}}
	free := func() (entryFacts, error) {
		return entryFacts{privileged: true, interfaces: up}, nil
	}
	member.SetEntryPreflight("192.0.2.10", "eth0", true, free)
	if err := member.prepareEntry(); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != uid {
		t.Fatalf("a finished preflight changed uid from %d to %d", uid, os.Geteuid())
	}
	t.Log("a free address on an up interface, with privilege and a network that can move it, passes and the user stays the same")

	member.SetEntryPreflight("not-an-ip", "eth0", true, free)
	expectEntryPreflight(t, member, EntryReasonAddress, "entry address is not an IP address")
	t.Log("an unusable entry address fails the address check")

	member.SetEntryPreflight("192.0.2.10", "eth0", true, func() (entryFacts, error) {
		return entryFacts{privileged: true, interfaces: []entryIface{{name: "eth0", up: true, addrs: []string{"192.0.2.10/24"}}}}, nil
	})
	expectEntryPreflight(t, member, EntryReasonAddress, "entry address is already present on a local interface")
	t.Log("an address that is already present on the chosen interface is an address conflict")

	member.SetEntryPreflight("192.0.2.10", "eth0", true, func() (entryFacts, error) {
		return entryFacts{privileged: true, interfaces: []entryIface{
			{name: "eth0", up: true},
			{name: "eth1", up: true, addrs: []string{"192.0.2.10"}},
		}}, nil
	})
	expectEntryPreflight(t, member, EntryReasonAddress, "entry address is already present on a local interface")
	t.Log("an address that is already present on another interface is an address conflict")

	member.SetEntryPreflight("192.0.2.10", "eth0", true, func() (entryFacts, error) {
		return entryFacts{privileged: true, interfaces: []entryIface{{name: "eth0", up: false}}}, nil
	})
	expectEntryPreflight(t, member, EntryReasonInterface, "entry interface is missing or down")
	member.SetEntryPreflight("192.0.2.10", "missing0", true, free)
	expectEntryPreflight(t, member, EntryReasonInterface, "entry interface is missing or down")
	t.Log("a down interface and a missing interface both fail the interface check")

	member.SetEntryPreflight("192.0.2.10", "eth0", true, func() (entryFacts, error) {
		return entryFacts{privileged: false, interfaces: up}, nil
	})
	expectEntryPreflight(t, member, EntryReasonPrivilege, "entry address needs network privilege and the server keeps its current user")
	if os.Geteuid() != uid {
		t.Fatalf("privilege result changed uid from %d to %d", uid, os.Geteuid())
	}
	t.Log("missing network privilege fails the check and the server keeps its current user")

	member.SetEntryPreflight("192.0.2.10", "eth0", false, free)
	expectEntryPreflight(t, member, EntryReasonNetwork, "this network does not allow the entry address to move")
	t.Log("a network that cannot move the address fails the network check")
}

func expectEntryPreflight(t *testing.T, member *App, reason, text string) {
	t.Helper()
	err := member.Run(context.Background())
	var got *EntryPreflightError
	if !errors.As(err, &got) || got.Reason != reason || got.Error() != text {
		t.Fatalf("Run err = %v, want reason %s %q", err, reason, text)
	}
}
