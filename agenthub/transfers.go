// SPDX-License-Identifier: Apache-2.0
package agenthub

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
)

// transferTable holds artifact readers between the control plane sending an
// InstallArtifact command and the agent pulling the bytes back via
// FetchArtifact. Handles are single-use and unguessable; the reader is
// consumed by exactly one fetch.
type transferTable struct {
	mu sync.Mutex
	m  map[string]io.Reader
}

func newTransferTable() *transferTable {
	return &transferTable{m: make(map[string]io.Reader)}
}

// register stores the reader under a fresh single-use id.
func (t *transferTable) register(r io.Reader) string {
	var b [16]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	t.mu.Lock()
	t.m[id] = r
	t.mu.Unlock()
	return id
}

// take removes and returns the reader; second takes fail (single-use).
func (t *transferTable) take(id string) (io.Reader, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.m[id]
	if !ok {
		return nil, fmt.Errorf("unknown or already-consumed transfer %q", id)
	}
	delete(t.m, id)
	return r, nil
}

// drop removes a registration that will never be fetched (install failed
// before the agent pulled).
func (t *transferTable) drop(id string) {
	t.mu.Lock()
	delete(t.m, id)
	t.mu.Unlock()
}
