//go:build dqlite && nosqlite3

package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"github.com/canonical/go-dqlite/v3/driver"
)

// This CLI is used only against authorized isolated HA test data directories.
const testDatabase = "gpuworks.db"

func main() {
	if err := run(); err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": err.Error()})
		os.Exit(1)
	}
}

func run() error {
	data := flag.String("data", "", "server data directory containing cluster-enrollment TLS files")
	address := flag.String("address", "", "seed member HOST:PORT")
	allowEmpty := flag.Bool("allow-empty", false, "return successful empty results for absence assertions")
	localOnly := flag.Bool("local-only", false, "reject every discovered endpoint other than --address")
	timeout := flag.Duration("timeout", 10*time.Second, "total operation timeout")
	action := flag.String("action", "inspect", "inspect, write, read, or transfer")
	note := flag.String("note", "", "exact test row note")
	targetID := flag.Uint64("target-id", 0, "voter ID receiving leadership")
	flag.Parse()
	if flag.NArg() != 0 || *data == "" || *address == "" || *timeout <= 0 {
		return errors.New("--data, --address and a positive --timeout are required")
	}
	if _, _, err := net.SplitHostPort(*address); err != nil {
		return fmt.Errorf("invalid seed address: %w", err)
	}
	switch *action {
	case "inspect", "write", "read", "transfer":
	default:
		return errors.New("unknown --action")
	}
	if (*action == "write" || *action == "read") && *note == "" {
		return errors.New("write and read require a nonempty --note")
	}
	if *action == "transfer" && *targetID == 0 {
		return errors.New("transfer requires a nonzero --target-id")
	}
	tlsConfig, err := loadTLS(*data)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	// Retain sockets until actual close, including discovery sockets donated to
	// the SQL connector. A context deadline also bounds protocol cleanup calls.
	var mu sync.Mutex
	var sockets []net.Conn
	dial := func(callCtx context.Context, requested string) (net.Conn, error) {
		if *localOnly && requested != *address {
			return nil, fmt.Errorf("local-only rejects changed endpoint %q", requested)
		}
		conn, err := (&tls.Dialer{Config: tlsConfig.Clone()}).DialContext(callCtx, "tcp", requested)
		if err != nil {
			return nil, err
		}
		deadline, _ := ctx.Deadline()
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, err
		}
		mu.Lock()
		sockets = append(sockets, conn)
		mu.Unlock()
		if err := ctx.Err(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
	closeSockets := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range sockets {
			_ = conn.Close()
		}
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { closeSockets(); close(closed) })
	defer func() {
		if !stop() {
			<-closed
		}
		closeSockets()
	}()
	store := client.NewInmemNodeStore()
	if err := store.Set(ctx, []client.NodeInfo{{Address: *address, Role: client.Voter}}); err != nil {
		return err
	}
	c, err := client.FindLeader(ctx, store, client.WithDialFunc(dial))
	if err != nil {
		return fmt.Errorf("find leader: %w", err)
	}
	defer c.Close()
	leader, err := c.Leader(ctx)
	if err != nil {
		return err
	}
	members, err := c.Cluster(ctx)
	if err != nil {
		return err
	}
	result := map[string]any{"action": *action, "seed": *address, "leader": publicMember(*leader)}
	publicMembers := make([]map[string]any, 0, len(members))
	for _, member := range members {
		publicMembers = append(publicMembers, publicMember(member))
	}
	result["members"] = publicMembers
	if *action == "inspect" {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if *action == "transfer" {
		valid := false
		for _, member := range members {
			if member.ID == *targetID && member.Role == client.Voter {
				valid = true
			}
		}
		if !valid {
			return errors.New("target ID is not an existing voter")
		}
		if err := c.Transfer(ctx, *targetID); err != nil {
			return fmt.Errorf("transfer: %w", err)
		}
		result["target_id"] = *targetID
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if err := store.Set(ctx, members); err != nil {
		return err
	}
	d, err := driver.New(store, driver.WithDialFunc(dial))
	if err != nil {
		return err
	}
	connector, err := d.OpenConnector(testDatabase)
	if err != nil {
		return err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	defer db.Close()
	result["database"], result["note"] = testDatabase, *note
	if *action == "write" {
		if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS gpuworks_catchup(id INTEGER PRIMARY KEY,note TEXT)"); err != nil {
			return fmt.Errorf("create test table: %w", err)
		}
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		id := int64(binary.LittleEndian.Uint64(random[:]) & (1<<63 - 1))
		if _, err := db.ExecContext(ctx, "INSERT INTO gpuworks_catchup(id,note) VALUES(?,?)", id, *note); err != nil {
			return fmt.Errorf("insert test row: %w", err)
		}
		result["id"] = id
	} else {
		rows, err := db.QueryContext(ctx, "SELECT id,note FROM gpuworks_catchup WHERE note=? ORDER BY id", *note)
		if err != nil {
			return fmt.Errorf("read test rows: %w", err)
		}
		defer rows.Close()
		matches := make([]map[string]any, 0)
		for rows.Next() {
			var id int64
			var matchedNote string
			if err := rows.Scan(&id, &matchedNote); err != nil {
				return err
			}
			matches = append(matches, map[string]any{"id": id, "note": matchedNote})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		result["rows"] = matches
		result["count"] = len(matches)
		if len(matches) == 0 && !*allowEmpty {
			return fmt.Errorf("no row found for note %q", *note)
		}
		var userCount int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&userCount); err != nil {
			result["users"] = map[string]any{"available": false, "error": err.Error()}
		} else {
			result["users"] = map[string]any{"available": true, "count": userCount}
		}
		var clusterID string
		if err := db.QueryRowContext(ctx, "SELECT cluster_id FROM cluster_identities WHERE id=1").Scan(&clusterID); err != nil {
			result["cluster_identity"] = map[string]any{"available": false, "error": err.Error()}
		} else {
			result["cluster_identity"] = map[string]any{"available": true, "cluster_id": clusterID}
		}
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func publicMember(member client.NodeInfo) map[string]any {
	return map[string]any{"id": member.ID, "address": member.Address, "role": member.Role.String()}
}

func loadTLS(data string) (*tls.Config, error) {
	enrollment := filepath.Join(data, "cluster-enrollment")
	ca, err := os.ReadFile(filepath.Join(enrollment, "ca.pem"))
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("CA file contains no valid certificates")
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(enrollment, "server.pem"), filepath.Join(enrollment, "server.key"))
	if err != nil {
		return nil, fmt.Errorf("load member certificate: %w", err)
	}
	// tls.Dialer derives ServerName from each actual target, preserving SAN
	// validation when leader discovery redirects from the initial seed.
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{pair}}, nil
}
