package alerts

import "time"

// NextCursor exposes nextCursor to the external tests.
func NextCursor(cursor, newest time.Time, full bool) time.Time {
	return nextCursor(cursor, newest, full)
}
