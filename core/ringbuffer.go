package core

import (
	"sync"
)

type RingBuffer struct {
	mu     sync.Mutex
	size   int
	events []any
	index  int
	full   bool
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		size:   size,
		events: make([]any, size),
	}
}

func (r *RingBuffer) Add(event any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events[r.index] = event
	r.index++

	if r.index >= r.size {
		r.index = 0
		r.full = true
	}
}

func (r *RingBuffer) Snapshot() []any {
	r.mu.Lock()
	defer r.mu.Unlock()

	var result []any

	if !r.full {
		result = append(result, r.events[:r.index]...)
		return result
	}

	result = append(result, r.events[r.index:]...)
	result = append(result, r.events[:r.index]...)

	return result
}
