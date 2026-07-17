// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"errors"
	"sync"
)

type scope struct {
	broadcaster *broadcaster
	mcp         EditorMCPSever
}

type registry struct {
	syncMap sync.Map
}

func newRegistry() *registry {
	return &registry{}
}

func (self *registry) remember(pid int, mcp EditorMCPSever) {
	broadcaster := newBroadcaster()

	self.syncMap.Store(pid, scope{broadcaster, mcp})
}

func (self *registry) mcp(pid int) (EditorMCPSever, error) {
	v, ok := self.syncMap.Load(pid)
	if !ok {
		return nil, errors.New("no endpoint registered")
	}
	s := v.(scope)
	return s.mcp, nil
}

func (self *registry) listen(pid int) (*listener, func(), error) {
	value, ok := self.syncMap.Load(pid)
	if !ok {
		return nil, nil, errors.New("no endpoint registered")
	}

	s := value.(scope)
	l, ch := s.broadcaster.listen()
	return l, ch, nil
}

func (self *registry) notify(pid int, v any) error {
	value, ok := self.syncMap.Load(pid)

	if !ok {
		return errors.New("unknown pid")
	}

	s := value.(scope)
	s.broadcaster.push(v)

	return nil
}
