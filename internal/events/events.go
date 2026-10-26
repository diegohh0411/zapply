// Package events defines the wire format shared by the relay, which publishes
// outbox rows to NATS, and the SSE gateway, which delivers them to clients.
// Design: docs/parts/03-events.md.
package events

import (
	"encoding/json"

	"github.com/google/uuid"
)

// Event is the NATS message body. Seq is the SSE id: it increases in publish
// order, and clients resume with seq > Last-Event-ID.
type Event struct {
	Seq   int64           `json:"seq"`
	Topic string          `json:"topic"`
	Data  json.RawMessage `json:"data"`
}

// RelayLockKey is the relay's transaction-scoped advisory lock. The relay
// holds it exclusively while it publishes a batch and commits. Replay takes it
// shared, so it waits for an in-flight batch to commit: the relay publishes
// before commit, so a stream that subscribed after the publish could
// otherwise replay before the commit and miss the event in both places.
const RelayLockKey int64 = 0x7a61_7070_6c79_0003

// Subject is where the relay publishes a user's event, e.g.
// user.0192….evidence.updated. Topics are dot-separated NATS tokens.
func Subject(userID uuid.UUID, topic string) string {
	return "user." + userID.String() + "." + topic
}

// UserSubjects matches every event for one user.
func UserSubjects(userID uuid.UUID) string {
	return "user." + userID.String() + ".>"
}
