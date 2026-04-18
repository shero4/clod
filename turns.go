package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Turn is a single MCP-initiated prompt waiting for claude to call
// submit_result on the internal MCP endpoint.
type Turn struct {
	ID       string
	resultCh chan turnResult
	done     bool
}

type turnResult struct {
	text string
	err  error
}

type TurnRegistry struct {
	mu    sync.Mutex
	turns map[string]*Turn
}

func NewTurnRegistry() *TurnRegistry {
	return &TurnRegistry{turns: make(map[string]*Turn)}
}

func (r *TurnRegistry) Create() *Turn {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	t := &Turn{
		ID:       hex.EncodeToString(b),
		resultCh: make(chan turnResult, 1),
	}
	r.mu.Lock()
	r.turns[t.ID] = t
	r.mu.Unlock()
	return t
}

func (r *TurnRegistry) Complete(id, text string) error {
	r.mu.Lock()
	t, ok := r.turns[id]
	if !ok {
		r.mu.Unlock()
		return errors.New("unknown turn_id")
	}
	if t.done {
		r.mu.Unlock()
		return errors.New("turn already completed")
	}
	t.done = true
	r.mu.Unlock()
	t.resultCh <- turnResult{text: text}
	return nil
}

func (r *TurnRegistry) Forget(id string) {
	r.mu.Lock()
	delete(r.turns, id)
	r.mu.Unlock()
}

// Wait blocks until the turn is completed or timeout elapses.
func (t *Turn) Wait(timeout time.Duration) (string, error) {
	select {
	case res := <-t.resultCh:
		return res.text, res.err
	case <-time.After(timeout):
		return "", errors.New("timed out waiting for submit_result")
	}
}
