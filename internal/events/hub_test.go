package events_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/events"
	"github.com/diegohh0411/zapply/internal/testnats"
)

func hub(t *testing.T, ns *testnats.Server, cfg events.HubConfig) *events.Hub {
	t.Helper()
	h, err := events.Connect(ns.ClientURL(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func publish(t *testing.T, ns *testnats.Server, uid uuid.UUID, seqs ...int64) {
	t.Helper()
	nc := ns.Connect(t)
	for _, seq := range seqs {
		msg, _ := json.Marshal(events.Event{Seq: seq, Topic: "evidence.updated", Data: json.RawMessage(`{}`)})
		if err := nc.Publish(events.Subject(uid, "evidence.updated"), msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

// drain reads until C closes and returns the seqs received.
func drain(t *testing.T, s *events.Stream) []int64 {
	t.Helper()
	var got []int64
	for {
		select {
		case e, ok := <-s.C:
			if !ok {
				return got
			}
			got = append(got, e.Seq)
		case <-time.After(3 * time.Second):
			t.Fatalf("stream still open; received %v", got)
		}
	}
}

func TestStreamsShareASubscriptionInOrder(t *testing.T) {
	ns := testnats.New(t)
	h := hub(t, ns, events.HubConfig{})
	uid := uuid.New()
	a, err := h.Subscribe(uid)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Subscribe(uid)
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.Subscribe(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	publish(t, ns, uid, 1, 2, 3)
	for _, s := range []*events.Stream{a, b} {
		for want := int64(1); want <= 3; want++ {
			select {
			case e := <-s.C:
				if e.Seq != want {
					t.Fatalf("got seq %d, want %d", e.Seq, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("event not delivered")
			}
		}
	}
	select {
	case e := <-other.C:
		t.Fatalf("another user's stream got %+v", e)
	case <-time.After(100 * time.Millisecond):
	}

	a.Close()
	a.Close() // idempotent
	publish(t, ns, uid, 4)
	if e := <-b.C; e.Seq != 4 {
		t.Fatalf("remaining stream got %d, want 4", e.Seq)
	}
}

func TestSlowStreamIsClosedNotStalled(t *testing.T) {
	ns := testnats.New(t)
	h := hub(t, ns, events.HubConfig{Buffer: 2})
	uid := uuid.New()
	slow, err := h.Subscribe(uid)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, ns, uid, 1, 2, 3, 4)
	// Don't read until all four have been delivered to the hub.
	for deadline := time.Now().Add(2 * time.Second); len(slow.C) < 2; {
		if time.Now().After(deadline) {
			t.Fatal("events not delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := drain(t, slow); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("slow stream got %v, want the 2 buffered events and then close", got)
	}
	// The user can open a new stream and replay.
	if _, err := h.Subscribe(uid); err != nil {
		t.Fatal(err)
	}
}

func TestStreamLimitPerUser(t *testing.T) {
	ns := testnats.New(t)
	h := hub(t, ns, events.HubConfig{MaxStreamsPerUser: 2})
	uid := uuid.New()
	s1, _ := h.Subscribe(uid)
	if _, err := h.Subscribe(uid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe(uid); !errors.Is(err, events.ErrTooManyStreams) {
		t.Fatalf("third stream: err = %v, want ErrTooManyStreams", err)
	}
	s1.Close()
	if _, err := h.Subscribe(uid); err != nil {
		t.Fatalf("after close: %v", err)
	}
}

func TestDisconnectClosesStreams(t *testing.T) {
	ns := testnats.New(t)
	h := hub(t, ns, events.HubConfig{})
	s, err := h.Subscribe(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	ns.Shutdown()
	drain(t, s)
	if _, err := h.Subscribe(uuid.New()); !errors.Is(err, events.ErrUnavailable) {
		t.Fatalf("subscribe while down: err = %v, want ErrUnavailable", err)
	}
}

func TestHubStartsWithoutNATS(t *testing.T) {
	ns := testnats.New(t)
	url := ns.ClientURL()
	ns.Shutdown()
	h, err := events.Connect(url, events.HubConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect with NATS down: %v", err)
	}
	defer h.Close()
	if _, err := h.Subscribe(uuid.New()); !errors.Is(err, events.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}
