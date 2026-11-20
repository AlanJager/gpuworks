package cluster

import (
	"context"
	"errors"
	"net"
	"sync"
)

type quorumSQLSocket struct {
	net.Conn
	once sync.Once
	err  error
}

func (c *quorumSQLSocket) Close() error {
	c.once.Do(func() { c.err = c.Conn.Close() })
	return c.err
}

type quorumSQLSockets struct {
	mu      sync.Mutex
	dials   sync.WaitGroup
	life    context.Context
	cancel  context.CancelFunc
	closed  bool
	sockets []*quorumSQLSocket
}

func (o *quorumSQLSockets) dial(ctx context.Context, address string, dial func(context.Context, string) (net.Conn, error)) (net.Conn, error) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil, errors.New("quorum SQL socket owner is closed")
	}
	o.dials.Add(1)
	o.mu.Unlock()
	defer o.dials.Done()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	stop := context.AfterFunc(o.life, func() { cancel(); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
		cancel()
	}()
	conn, err := dial(ctx, address)
	if conn == nil {
		if err == nil {
			err = errors.New("quorum SQL dial returned no connection")
		}
		return nil, errors.Join(err, ctx.Err())
	}
	owned := &quorumSQLSocket{Conn: conn}
	o.mu.Lock()
	o.sockets = append(o.sockets, owned)
	closed := o.closed
	o.mu.Unlock()
	if err != nil || closed || ctx.Err() != nil {
		if closed {
			err = errors.Join(err, errors.New("quorum SQL socket owner is closed"))
		}
		return nil, errors.Join(err, ctx.Err(), owned.Close())
	}
	return owned, nil
}

func (o *quorumSQLSockets) Close() error {
	o.mu.Lock()
	o.closed = true
	o.cancel()
	sockets := append([]*quorumSQLSocket(nil), o.sockets...)
	o.mu.Unlock()
	// Wake pending SQL/ROLLBACK reads before waiting for outstanding dials.
	for _, socket := range sockets {
		_ = socket.Close()
	}
	o.dials.Wait()
	o.mu.Lock()
	sockets = append([]*quorumSQLSocket(nil), o.sockets...)
	o.mu.Unlock()
	var result error
	for _, socket := range sockets {
		result = errors.Join(result, socket.Close())
	}
	return result
}
