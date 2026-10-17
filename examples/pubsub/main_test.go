package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

func dial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), "ws"+srv.URL[len("http"):])
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func send(t *testing.T, conn *websocket.Conn, req envelope) {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(context.Background(), websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
}

func expect(t *testing.T, conn *websocket.Conn, want envelope) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, msg, err := conn.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	var got envelope
	if err := json.Unmarshal(msg, &got); err != nil || got != want {
		t.Fatalf("got %s (%v), want %+v", msg, err, want)
	}
}

func TestPublishReachesOnlySubscribers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(newBroker().handler))
	defer srv.Close()

	news, sports, publisher := dial(t, srv), dial(t, srv), dial(t, srv)
	send(t, news, envelope{Op: "subscribe", Topic: "news"})
	expect(t, news, envelope{Op: "subscribed", Topic: "news"})
	send(t, sports, envelope{Op: "subscribe", Topic: "sports"})
	expect(t, sports, envelope{Op: "subscribed", Topic: "sports"})

	send(t, publisher, envelope{Op: "publish", Topic: "news", Data: "extra"})
	send(t, publisher, envelope{Op: "publish", Topic: "sports", Data: "goal"})

	// Each connection delivers in order: had the sports subscriber been sent
	// the news item, it would read that here instead of the goal.
	expect(t, news, envelope{Op: "message", Topic: "news", Data: "extra"})
	expect(t, sports, envelope{Op: "message", Topic: "sports", Data: "goal"})
}

func TestUnsubscribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(newBroker().handler))
	defer srv.Close()

	sub, publisher := dial(t, srv), dial(t, srv)
	send(t, sub, envelope{Op: "subscribe", Topic: "a"})
	expect(t, sub, envelope{Op: "subscribed", Topic: "a"})
	send(t, sub, envelope{Op: "unsubscribe", Topic: "a"})
	// A second subscription doubles as a barrier: once its ack arrives the
	// server has processed the unsubscribe that came before it.
	send(t, sub, envelope{Op: "subscribe", Topic: "b"})
	expect(t, sub, envelope{Op: "subscribed", Topic: "b"})

	send(t, publisher, envelope{Op: "publish", Topic: "a", Data: "missed"})
	send(t, publisher, envelope{Op: "publish", Topic: "b", Data: "seen"})
	expect(t, sub, envelope{Op: "message", Topic: "b", Data: "seen"})
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	b := newBroker()
	slow := &client{send: make(chan []byte, 1)}
	b.subscribe(slow, "t") // the ack fills the queue
	b.publish("t", "overflow")

	if len(b.topics) != 0 {
		t.Fatalf("topics = %v, want none", b.topics)
	}
	<-slow.send
	if _, open := <-slow.send; open {
		t.Fatal("slow subscriber's queue is still open")
	}
	b.unsubscribe(slow, "t")
	b.subscribe(slow, "t") // must not send on the closed queue
	b.leave(slow)          // must not close it twice
	if len(b.dropped) != 0 {
		t.Fatal("leave did not forget the client")
	}
}
