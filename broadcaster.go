// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import "sync"

type broadcaster struct {
	next      int
	mu        sync.Mutex
	listeners map[int]chan any
}

func newBroadcaster() *broadcaster {
	return &broadcaster{listeners: make(map[int]chan any)}
}

type listener struct {
	ch chan any
}

func (b *broadcaster) listen() (*listener, func()) {
	b.mu.Lock()
	id := b.next
	b.next++
	defer b.mu.Unlock()

	ch := make(chan any, 16)
	b.listeners[id] = ch
	return &listener{ch: ch}, func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		delete(b.listeners, id)
	}
}

func (b *broadcaster) push(v any) {
	b.mu.Lock()
	ls := make([]chan any, 0, len(b.listeners))
	for _, ch := range b.listeners {
		ls = append(ls, ch)
	}
	b.mu.Unlock()

	for _, ch := range ls {
		ch <- v
	}
}

// recv exposes the listener's channel as receive-only, so callers can select on
// it (e.g. against a context's Done channel) without being able to push into it
// — only the broadcaster pushes.
func (l *listener) recv() <-chan any {
	return l.ch
}
