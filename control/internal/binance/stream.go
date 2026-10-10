package binance

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

// StreamEvent is the subset of a user data stream message LossGuard reacts to.
type StreamEvent struct {
	Type        string // ORDER_TRADE_UPDATE, ACCOUNT_UPDATE, ALGO_UPDATE, ...
	Symbol      string
	OrderStatus string
}

type rawEvent struct {
	E string `json:"e"`
	O struct {
		S string `json:"s"`
		X string `json:"X"`
	} `json:"o"`
}

const keepAliveEvery = 30 * time.Minute

// RunUserStream keeps a user data stream open until ctx is cancelled, reconnecting on
// any error, and calls onEvent for every account/order event pushed by Binance. This is
// what lets the block react to a new order within a second instead of waiting for the
// next poll.
func (c *Client) RunUserStream(ctx context.Context, wsBase string, onEvent func(StreamEvent)) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.streamOnce(ctx, wsBase, onEvent)
		if ctx.Err() != nil {
			return
		}
		log.Printf("user stream: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (c *Client) streamOnce(ctx context.Context, wsBase string, onEvent func(StreamEvent)) error {
	key, err := c.NewListenKey(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.CloseListenKey(cctx)
	}()

	// Since 2026-03 private data is served on the routed /private path; the legacy
	// /ws path is kept as a fallback.
	var conn *websocket.Conn
	for _, p := range []string{"/private/ws/", "/ws/"} {
		if conn, _, err = websocket.DefaultDialer.DialContext(ctx, wsBase+p+key, nil); err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	defer conn.Close()

	// Closing the connection unblocks ReadMessage when ctx ends.
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(keepAliveEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			case <-done:
				return
			case <-t.C:
				kctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := c.KeepAliveListenKey(kctx); err != nil {
					log.Printf("user stream keepalive: %v", err)
				}
				cancel()
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var ev rawEvent
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}
		if ev.E == "listenKeyExpired" {
			return errListenKeyExpired
		}
		onEvent(StreamEvent{Type: ev.E, Symbol: ev.O.S, OrderStatus: ev.O.X})
	}
}

type streamError string

func (e streamError) Error() string { return string(e) }

const errListenKeyExpired = streamError("listen key expired")
