package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestHubRouting(t *testing.T) {
	hub := NewHub()
	a := make(chan Event, 2)
	b := make(chan Event, 2)
	hub.Subscribe("site1", "posts", a)
	hub.Subscribe("site2", "posts", b)

	hub.Publish("site1", "posts", Event{Type: "create", ID: "x"})
	if len(a) != 1 {
		t.Fatalf("subscriber a got %d events, want 1", len(a))
	}
	if ev := <-a; ev.Type != "create" || ev.ID != "x" {
		t.Errorf("subscriber a got %+v", ev)
	}
	if len(b) != 0 {
		t.Errorf("subscriber b (other scope) got %d events, want 0", len(b))
	}

	// Same scope+collection reaches all subscribers — the shared-* case.
	c := make(chan Event, 2)
	hub.Subscribe("_shared", "shared-libs", a)
	hub.Subscribe("_shared", "shared-libs", c)
	hub.Publish("_shared", "shared-libs", Event{Type: "create", ID: "y"})
	if len(a) != 1 || len(c) != 1 {
		t.Errorf("shared publish reached a=%d c=%d subscribers, want 1 each", len(a), len(c))
	}
}

func TestHubUnsubscribeAndFullBuffers(t *testing.T) {
	hub := NewHub()
	a := make(chan Event, 1)
	hub.Subscribe("site1", "posts", a)
	hub.UnsubscribeAll(a)
	hub.Publish("site1", "posts", Event{Type: "create"})
	if len(a) != 0 {
		t.Errorf("unsubscribed channel got %d events, want 0", len(a))
	}

	// A full buffer drops events instead of blocking the fan-out.
	full := make(chan Event, 1)
	hub.Subscribe("site1", "posts", full)
	hub.Publish("site1", "posts", Event{Type: "create", ID: "1"})
	hub.Publish("site1", "posts", Event{Type: "create", ID: "2"})
	if len(full) != 1 {
		t.Errorf("full channel holds %d events, want 1 (second dropped)", len(full))
	}
}

func TestDisconnectScopeRevokesSessionsWithoutClosingReusableChannels(t *testing.T) {
	hub := NewHub()
	docOut := make(chan Event, 4)
	revoked := make(chan struct{})
	if !hub.RegisterSession("site1", "session1", hub.SiteEpoch("site1"), docOut, func() { close(revoked) }) {
		t.Fatal("register session")
	}
	hub.Subscribe("site1", "posts", docOut)
	hub.Subscribe("_shared", "shared-posts", docOut)

	hub.DisconnectScope("site1")
	select {
	case <-revoked:
	default:
		t.Fatal("site session was not revoked")
	}
	hub.Publish("_shared", "shared-posts", Event{Type: "create"})
	if len(docOut) != 0 {
		t.Fatalf("revoked session received %d shared events, want 0", len(docOut))
	}
	if hub.RegisterSession("site1", "late", 0, docOut, func() {}) {
		t.Fatal("session registered with an epoch captured before revocation")
	}

	// A request already queued at revocation may try to subscribe before
	// the handler observes cancellation. The channel must remain safe.
	hub.Subscribe("site1", "posts", docOut)
	hub.Publish("site1", "posts", Event{Type: "create"})
	if len(docOut) != 1 {
		t.Fatalf("reused channel received %d events, want 1", len(docOut))
	}

	rooms := NewRoomHub()
	roomOut := make(chan RoomEvent, 8)
	user := RoomUser{ID: "session1"}
	rooms.Join("site1", "control", user, roomOut)
	rooms.Join("_shared", "shared-control", user, roomOut)
	for len(roomOut) > 0 {
		<-roomOut
	}
	rooms.DisconnectScope("site1")
	rooms.Join("site1", "control", user, roomOut)
	if len(roomOut) != 1 {
		t.Fatalf("reused room channel received %d events, want presence", len(roomOut))
	}
}

func TestDisconnectScopeClosesIdleAndSharedOnlyWebSockets(t *testing.T) {
	for _, tt := range []struct {
		name      string
		subscribe bool
	}{
		{name: "idle"},
		{name: "shared-only", subscribe: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{policies: NewPolicyStore(t.TempDir(), 0), spotDomain: "spot.localhost"}
			ts := httptest.NewServer(srv.routes())
			defer ts.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws", &websocket.DialOptions{
				HTTPHeader: http.Header{"X-Forwarded-Host": []string{"demo.spot.localhost"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if tt.subscribe {
				if err := wsjson.Write(ctx, conn, wsRequest{Type: "subscribe", Collection: "shared-posts"}); err != nil {
					t.Fatal(err)
				}
				var ack map[string]string
				if err := wsjson.Read(ctx, conn, &ack); err != nil || ack["type"] != "subscribed" {
					t.Fatalf("subscribe ack = %v, %v", ack, err)
				}
			} else {
				deadline := time.Now().Add(time.Second)
				for {
					srv.hub.mu.Lock()
					registered := len(srv.hub.sessions["demo"]) == 1
					srv.hub.mu.Unlock()
					if registered {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("websocket session was not registered")
					}
					time.Sleep(time.Millisecond)
				}
			}

			srv.disconnectSiteRealtime("demo")
			readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
			defer readCancel()
			var message any
			if err := wsjson.Read(readCtx, conn, &message); err == nil {
				t.Fatalf("revoked websocket stayed open and read %#v", message)
			}
		})
	}
}
