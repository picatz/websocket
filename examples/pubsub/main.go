// Command pubsub is a topic-based publish/subscribe server.
//
// Clients send JSON requests as text messages:
//
//	{"op":"subscribe","topic":"news"}
//	{"op":"unsubscribe","topic":"news"}
//	{"op":"publish","topic":"news","data":"hello"}
//
// The server acknowledges subscriptions with {"op":"subscribed","topic":...}
// and delivers each publication to that topic's subscribers as
// {"op":"message","topic":...,"data":...}. Publishers need not subscribe.
//
// As in the chat example, each client has a bounded send queue drained by its
// own writer goroutine, and a client that falls behind is disconnected.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/picatz/websocket"
)

const (
	sendQueue  = 64
	writeWait  = 5 * time.Second
	maxMessage = 64 << 10
)

// envelope is the JSON shape of every message in both directions.
type envelope struct {
	Op    string `json:"op"`
	Topic string `json:"topic"`
	Data  string `json:"data,omitempty"`
}

type client struct {
	send chan []byte
}

// broker maps topics to their subscribers.
type broker struct {
	mu     sync.Mutex
	topics map[string]map[*client]struct{}
	// dropped holds clients removed for being slow, so that later
	// unsubscribes and the final cleanup leave their closed queue alone.
	dropped map[*client]struct{}
}

func newBroker() *broker {
	return &broker{
		topics:  make(map[string]map[*client]struct{}),
		dropped: make(map[*client]struct{}),
	}
}

// subscribe adds c to topic and queues the acknowledgement. The ack is queued
// under the same lock as the subscription, so c sees it before any message
// published after subscribe returns.
func (b *broker) subscribe(c *client, topic string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, gone := b.dropped[c]; gone {
		return
	}
	subs := b.topics[topic]
	if subs == nil {
		subs = make(map[*client]struct{})
		b.topics[topic] = subs
	}
	subs[c] = struct{}{}
	b.enqueue(c, envelope{Op: "subscribed", Topic: topic})
}

func (b *broker) unsubscribe(c *client, topic string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.remove(c, topic)
}

func (b *broker) remove(c *client, topic string) {
	delete(b.topics[topic], c)
	if len(b.topics[topic]) == 0 {
		delete(b.topics, topic)
	}
}

// publish queues data for every subscriber of topic.
func (b *broker) publish(topic, data string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.topics[topic] {
		b.enqueue(c, envelope{Op: "message", Topic: topic, Data: data})
	}
}

// enqueue queues msg for c, dropping c if its queue is full. b.mu is held.
func (b *broker) enqueue(c *client, msg envelope) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
		b.drop(c)
	}
}

// drop removes c from every topic and closes its queue. b.mu is held.
func (b *broker) drop(c *client) {
	if _, gone := b.dropped[c]; gone {
		return
	}
	for topic := range b.topics {
		b.remove(c, topic)
	}
	b.dropped[c] = struct{}{}
	close(c.send)
}

// leave drops c, if it is still connected, and forgets it.
func (b *broker) leave(c *client) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drop(c)
	delete(b.dropped, c)
}

func (b *broker) handler(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Upgrade(w, r, websocket.WithUpgradeMaxMessageSize(maxMessage))
	if err != nil {
		// Upgrade has already written the HTTP error response.
		return
	}
	defer conn.Close()

	c := &client{send: make(chan []byte, sendQueue)}
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		defer conn.Close()
		for msg := range c.send {
			ctx, cancel := context.WithTimeout(r.Context(), writeWait)
			err := conn.WriteMessageContext(ctx, websocket.TextMessage, msg)
			cancel()
			if err != nil {
				return
			}
		}
	})
	defer b.leave(c)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req envelope
		if err := json.Unmarshal(msg, &req); err != nil || req.Topic == "" {
			continue
		}
		switch req.Op {
		case "subscribe":
			b.subscribe(c, req.Topic)
		case "unsubscribe":
			b.unsubscribe(c, req.Topic)
		case "publish":
			b.publish(req.Topic, req.Data)
		}
	}
}

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()

	http.HandleFunc("/ws", newBroker().handler)
	log.Printf("pub/sub server listening on ws://%s/ws", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
