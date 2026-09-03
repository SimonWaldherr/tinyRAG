package main

import (
	"crypto/rand"
	"fmt"
	"time"
)

// newRequestID generates a short random identifier used for load IDs,
// request IDs and other opaque handles.
func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err == nil {
		return fmt.Sprintf("%x", b)
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}
