package registryd

import (
	"context"
	"errors"
	"sync"

	"github.com/fables-for-robots/amber-store-core/key"
	"github.com/fables-for-robots/amber-store-iroh/protocol"

	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/amberclient"
)

// reconnSync wraps amberclient with redial-on-failure (the runnerd pattern):
// a Client is one connection, so when an operation fails on transport (server
// restart, link drop) the client is discarded and the operation retried once
// on a fresh dial. Errors the SERVER answered with (protocol.RemoteError,
// ErrRefNotFound) never redial — the connection just proved itself.
type reconnSync struct {
	opts amberclient.Options
	st   *amber.Store

	mu  sync.Mutex
	cur *amberclient.Client
}

func newReconnSync(opts amberclient.Options, st *amber.Store) *reconnSync {
	return &reconnSync{opts: opts, st: st}
}

func (r *reconnSync) Pull(ctx context.Context, name string) (key.Key, error) {
	var root key.Key
	err := r.do(ctx, func(c *amberclient.Client) error {
		var err error
		root, err = c.Pull(ctx, r.st, name)
		return err
	})
	return root, err
}

func (r *reconnSync) Refs(ctx context.Context) ([]amberclient.RefInfo, error) {
	var refs []amberclient.RefInfo
	err := r.do(ctx, func(c *amberclient.Client) error {
		var err error
		refs, err = c.Refs(ctx)
		return err
	})
	return refs, err
}

// do runs op on the current client, dialing lazily and retrying once on a
// fresh connection after a transport failure.
//
// An op that failed because its OWN context expired must not drop the client:
// unlike runnerd, registryd feeds short-lived HTTP request contexts in here
// (tags listings) alongside long assembly pulls on the same shared
// connection, and a cancelled listing says nothing about the connection's
// health — closing it would abort every concurrent pull.
func (r *reconnSync) do(ctx context.Context, op func(*amberclient.Client) error) error {
	for attempt := 0; ; attempt++ {
		c, err := r.client(ctx)
		if err != nil {
			return err
		}
		err = op(c)
		if err == nil || isRemoteVerdict(err) || ctx.Err() != nil {
			return err
		}
		r.drop(c)
		if attempt >= 1 {
			return err
		}
	}
}

// isRemoteVerdict reports whether the server itself answered err — proof the
// connection works, so redialing cannot change the outcome.
func isRemoteVerdict(err error) bool {
	var re *protocol.RemoteError
	return errors.As(err, &re) || errors.Is(err, amberclient.ErrRefNotFound)
}

func (r *reconnSync) client(ctx context.Context) (*amberclient.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil {
		return r.cur, nil
	}
	c, err := amberclient.Dial(ctx, r.opts)
	if err != nil {
		return nil, err
	}
	r.cur = c
	return c, nil
}

// drop closes c and forgets it (if still current).
func (r *reconnSync) drop(c *amberclient.Client) {
	r.mu.Lock()
	if r.cur == c {
		r.cur = nil
	}
	r.mu.Unlock()
	_ = c.Close()
}

// Close releases the current connection, if any.
func (r *reconnSync) Close() {
	r.mu.Lock()
	c := r.cur
	r.cur = nil
	r.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}
