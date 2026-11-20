//go:build linux && cgo && dqlite

// Command gpuworks-dqlite is the only cgo binary of a clustered control plane.
// It runs the dqlite node and the few offline Raft operations that need the C
// library. The server stays pure Go, starts this program as a child process
// from its own directory and speaks the dqlite wire protocol to it.
//
// Contract with the server:
//
//	gpuworks-dqlite --dir DIR --address HOST:PORT [--unix @NAME] [--seeds a,b]
//	                [--cert FILE --key FILE --ca FILE --policy FILE]
//	                [--network-latency-ms N]
//
// serves the node from the existing directory DIR. Once the node is listening
// it writes one JSON line {"address":"HOST:PORT","id":<uint64>} to standard
// output, closes standard output and runs until SIGTERM or SIGINT, then exits
// 0. The four TLS flags come together or not at all. With them the listener
// and outbound Raft connections use mutual TLS 1.3 against the pinned cluster
// authority, and every handshake is authorized against the transport policy
// file as it is at that moment; an unreadable policy refuses the handshake.
// --network-latency-ms selects the cluster's storage tier by the latency the
// node derives its election timeout from; only a tier's value is accepted and
// without the flag the node runs the standard tier.
//
// One-shot commands (standard output shown; exit 0 on success):
//
//	id --address HOST:PORT                        decimal member id and newline
//	describe-last --dir DIR                       {"index":N,"term":N}
//	reconfigure --dir DIR --id N --address A [--role voter|stand-by|spare]
//	reconfigure --dir DIR --member N,A,ROLE [--member ...]
//	isolated --dir DIR --id N --address A [--bind @NAME] [--reject TEXT]
//	                                              {"bind":"@NAME"}, then as serve
//	inspect --dir DIR --id N --address A --name DB  {"schema":"<sha256 hex>"}
//
// isolated starts the node without any outbound Raft connection and announces
// a socket this process owns; it forwards local connections of its own user to
// the node, because dqlite accepts its private socket only from this process.
//
// Every mode except id holds an exclusive lock on DIR+".helper.lock". Usage
// errors exit 2 and runtime failures exit 1, both with one line on standard
// error. Nothing else is written to standard output.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	dqlite "github.com/canonical/go-dqlite/v3"
	"github.com/canonical/go-dqlite/v3/app"
	"github.com/canonical/go-dqlite/v3/client"
	"github.com/canonical/go-dqlite/v3/driver"
	"gpuworks.ai/gpuworks/internal/filelock"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "id":
			return runID(args[1:])
		case "describe-last":
			return runDescribe(args[1:])
		case "reconfigure":
			return runReconfigure(args[1:])
		case "isolated":
			return runIsolated(args[1:])
		case "inspect":
			return runInspect(args[1:])
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
			return 2
		}
	}
	return runServe(args)
}

func runID(args []string) int {
	fs := flag.NewFlagSet("id", flag.ContinueOnError)
	address := fs.String("address", "", "")
	if err := fs.Parse(args); err != nil || *address == "" {
		fmt.Fprintln(os.Stderr, "usage: gpuworks-dqlite id --address HOST:PORT")
		return 2
	}
	fmt.Println(dqlite.GenerateID(*address))
	return 0
}

func runDescribe(args []string) int {
	fs := flag.NewFlagSet("describe-last", flag.ContinueOnError)
	dir := fs.String("dir", "", "")
	if err := fs.Parse(args); err != nil || *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: gpuworks-dqlite describe-last --dir DIR")
		return 2
	}
	return withDirLock(*dir, func() int {
		last, err := dqlite.ReadLastEntryInfo(*dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]uint64{"index": last.Index, "term": last.Term}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	})
}

type memberList []dqlite.NodeInfo

func (m *memberList) String() string { return fmt.Sprint(len(*m)) }

// Set parses one ID,ADDRESS,ROLE member of the configuration to write.
func (m *memberList) Set(value string) error {
	parts := strings.Split(value, ",")
	if len(parts) != 3 || parts[1] == "" {
		return fmt.Errorf("member %q is not ID,ADDRESS,ROLE", value)
	}
	id, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || id == 0 {
		return fmt.Errorf("member %q has no positive id", value)
	}
	role, err := parseRole(parts[2])
	if err != nil {
		return err
	}
	for _, existing := range *m {
		if existing.ID == id || existing.Address == parts[1] {
			return fmt.Errorf("member %q repeats an id or address", value)
		}
	}
	*m = append(*m, dqlite.NodeInfo{ID: id, Address: parts[1], Role: role})
	return nil
}

func runReconfigure(args []string) int {
	fs := flag.NewFlagSet("reconfigure", flag.ContinueOnError)
	dir := fs.String("dir", "", "")
	id := fs.Uint64("id", 0, "")
	address := fs.String("address", "", "")
	role := fs.String("role", "voter", "")
	var members memberList
	fs.Var(&members, "member", "")
	single := false
	err := fs.Parse(args)
	fs.Visit(func(f *flag.Flag) { single = single || f.Name == "id" || f.Name == "address" || f.Name == "role" })
	if err != nil || *dir == "" || (len(members) == 0) == (*id == 0 || *address == "") || (len(members) != 0 && single) {
		fmt.Fprintln(os.Stderr, "usage: gpuworks-dqlite reconfigure --dir DIR (--id ID --address HOST:PORT [--role voter] | --member ID,HOST:PORT,ROLE ...)")
		return 2
	}
	if len(members) == 0 {
		nodeRole, err := parseRole(*role)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		members = memberList{{ID: *id, Address: *address, Role: nodeRole}}
	}
	return withDirLock(*dir, func() int {
		if err := dqlite.ReconfigureMembershipExt(*dir, members); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	})
}

func runIsolated(args []string) int {
	fs := flag.NewFlagSet("isolated", flag.ContinueOnError)
	dir := fs.String("dir", "", "")
	id := fs.Uint64("id", 0, "")
	address := fs.String("address", "", "")
	bind := fs.String("bind", "@", "")
	reject := fs.String("reject", "offline node rejects outbound raft", "")
	if err := fs.Parse(args); err != nil || *dir == "" || *id == 0 || *address == "" || *bind == "" {
		fmt.Fprintln(os.Stderr, "usage: gpuworks-dqlite isolated --dir DIR --id ID --address HOST:PORT --bind @SOCKET")
		return 2
	}
	stop := stopSignals()
	return withDirLock(*dir, func() int {
		node, err := dqlite.New(*id, *address, *dir,
			dqlite.WithBindAddress(*bind),
			dqlite.WithAutoRecovery(false),
			dqlite.WithDialFunc(func(context.Context, string) (net.Conn, error) {
				return nil, errors.New(*reject)
			}))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer node.Close()
		if err := node.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		front, err := serveIsolatedFront(node.BindAddress())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer front.Close()
		if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"bind": front.Addr().String()}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = os.Stdout.Close()
		<-stop
		return 0
	})
}

func runInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	dir := fs.String("dir", "", "")
	id := fs.Uint64("id", 0, "")
	address := fs.String("address", "", "")
	name := fs.String("name", "", "")
	if err := fs.Parse(args); err != nil || *dir == "" || *id == 0 || *address == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "usage: gpuworks-dqlite inspect --dir DIR --id ID --address HOST:PORT --name DB")
		return 2
	}
	return withDirLock(*dir, func() int {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		node, err := dqlite.New(*id, *address, *dir,
			dqlite.WithBindAddress("@"),
			dqlite.WithAutoRecovery(false),
			dqlite.WithDialFunc(func(context.Context, string) (net.Conn, error) {
				return nil, errors.New("offline address verification rejects outbound Raft connections")
			}))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer node.Close()
		if err := node.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		schema, err := inspectBoundSchema(ctx, node.BindAddress(), *id, *address, *name)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"schema": schema}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	})
}

// inspectBoundSchema dials the helper's own abstract socket. dqlite's accept
// path allows that socket only for the process that bound it.
func inspectBoundSchema(ctx context.Context, socket string, id uint64, address, name string) (string, error) {
	if !strings.HasPrefix(socket, "@") || len(socket) < 2 {
		return "", errors.New("isolated source did not bind a private Unix socket")
	}
	dial := func(ctx context.Context, requested string) (net.Conn, error) {
		if requested != address {
			return nil, errors.New("isolated source rejects a different leader address")
		}
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	c, err := client.New(ctx, address, client.WithDialFunc(dial))
	if err != nil {
		return "", err
	}
	members, queryErr := c.Cluster(ctx)
	closeErr := c.Close()
	if err := errors.Join(queryErr, closeErr); err != nil {
		return "", err
	}
	if len(members) != 1 || members[0].ID != id || members[0].Address != address || members[0].Role != client.Voter {
		return "", fmt.Errorf("offline conversion requires exactly the original voter; actual Raft members: %+v", members)
	}
	seeds := client.NewInmemNodeStore()
	if err := seeds.Set(ctx, []client.NodeInfo{{ID: id, Address: address, Role: client.Voter}}); err != nil {
		return "", err
	}
	sqlDriver, err := driver.New(seeds, driver.WithDialFunc(dial))
	if err != nil {
		return "", err
	}
	connector, err := sqlDriver.OpenConnector(name)
	if err != nil {
		return "", err
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	var last error
	for {
		last = db.PingContext(ctx)
		if last == nil {
			break
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("database readiness: %w (last connection error: %v)", ctx.Err(), last)
		case <-timer.C:
		}
	}
	return schemaDigest(ctx, db)
}

// schemaDigest must stay identical to cluster.conversionSchema. The server
// recomputes that digest after address conversion and refuses a mismatch.
func schemaDigest(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema ORDER BY type, name")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hash := sha256.New()
	count := 0
	for rows.Next() {
		var row [4]string
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3]); err != nil {
			return "", err
		}
		if err := json.NewEncoder(hash).Encode(row); err != nil {
			return "", err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		return "", errors.New("address conversion refuses an empty application database")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("gpuworks-dqlite", flag.ContinueOnError)
	dir := fs.String("dir", "", "")
	address := fs.String("address", "", "")
	unixSocket := fs.String("unix", "", "")
	seeds := fs.String("seeds", "", "")
	certPath := fs.String("cert", "", "")
	keyPath := fs.String("key", "", "")
	caPath := fs.String("ca", "", "")
	policyPath := fs.String("policy", "", "")
	latencyMS := fs.Int("network-latency-ms", int(storagetier.Standard.NetworkLatency()/time.Millisecond), "")
	if err := fs.Parse(args); err != nil || *dir == "" || *address == "" {
		fmt.Fprintln(os.Stderr, "usage: gpuworks-dqlite --dir DIR --address HOST:PORT [--unix PATH] [--seeds a,b] [--cert --key --ca --policy] [--network-latency-ms N]")
		return 2
	}
	// Every member must elect on the same timing, so only the latency of a
	// named storage tier is accepted.
	latency := time.Duration(*latencyMS) * time.Millisecond
	if _, ok := storagetier.ForNetworkLatency(latency); !ok {
		fmt.Fprintln(os.Stderr, "--network-latency-ms must be the latency of a storage tier")
		return 2
	}
	if (*certPath == "") != (*keyPath == "") || (*certPath == "") != (*caPath == "") || (*certPath == "") != (*policyPath == "") {
		fmt.Fprintln(os.Stderr, "--cert, --key, --ca and --policy must be given together")
		return 2
	}
	stop := stopSignals()
	return withDirLock(*dir, func() int {
		opts := []app.Option{
			// Short overlapping writes from the server's ordinary and
			// scheduling pools wait for each other instead of failing.
			// Caller deadlines still bound SQL.
			app.WithBusyTimeout(5 * time.Second),
			app.WithAddress(*address),
			app.WithVoters(3),
			app.WithStandBys(0),
			app.WithNetworkLatency(latency),
			app.WithRolesAdjustmentFrequency(time.Second),
		}
		if *seeds != "" {
			opts = append(opts, app.WithCluster(strings.Split(*seeds, ",")))
		}
		if *unixSocket != "" {
			opts = append(opts, app.WithUnixSocket(*unixSocket))
		}
		if *certPath != "" {
			listen, dial, err := loadTransportTLS(*certPath, *keyPath, *caPath, *policyPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			opts = append(opts, app.WithTLS(listen, dial))
		}
		node, err := app.New(*dir, opts...)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer node.Close()
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"id": node.ID(), "address": node.Address()}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = os.Stdout.Close()
		<-stop
		return 0
	})
}

func withDirLock(dir string, fn func() int) int {
	release, err := filelock.Acquire(dir + ".helper.lock")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer release()
	return fn()
}

// stopSignals must be installed before the node starts. A server that stops
// the helper right after its announcement would otherwise kill it with the
// default action and skip the orderly node shutdown.
func stopSignals() <-chan os.Signal {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	return signals
}

func parseRole(role string) (client.NodeRole, error) {
	switch role {
	case "voter":
		return client.Voter, nil
	case "stand-by":
		return client.StandBy, nil
	case "spare":
		return client.Spare, nil
	default:
		return 0, fmt.Errorf("unknown member role %q", role)
	}
}
