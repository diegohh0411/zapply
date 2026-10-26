package events

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/testnats"
)

// NATS drops messages for a slow subscription without disconnecting. The
// streams it feeds must end so their clients replay, not continue with a gap.
func TestNATSSlowConsumerClosesStreams(t *testing.T) {
	ns := testnats.New(t)
	h, err := Connect(ns.ClientURL(), HubConfig{Buffer: 1000, PendingMsgs: 5},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	uid := uuid.New()
	s, err := h.Subscribe(uid)
	if err != nil {
		t.Fatal(err)
	}

	// Stall delivery so NATS's queue for the subscription overflows.
	h.mu.Lock()
	nc := ns.Connect(t)
	for seq := range int64(50) {
		msg, _ := json.Marshal(Event{Seq: seq + 1, Topic: "evidence.updated", Data: json.RawMessage(`{}`)})
		if err := nc.Publish(Subject(uid, "evidence.updated"), msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	h.mu.Unlock()

	var got int
	for {
		select {
		case _, ok := <-s.C:
			if !ok {
				if got >= 50 {
					t.Fatalf("received all %d events; expected drops", got)
				}
				return
			}
			got++
		case <-time.After(3 * time.Second):
			t.Fatalf("stream still open after NATS dropped events (received %d of 50)", got)
		}
	}
}
