package main

import (
	"errors"
	"sync"
)

type scope struct {
	endpoint    string
	broadcaster *broadcaster
}

type registry struct {
	syncMap sync.Map
}

func newRegistry() *registry {
	return &registry{}
}

func (self *registry) remember(pid int, endpoint string) {
	broadcaster := newBroadcaster()
	self.syncMap.Store(pid, scope{endpoint, broadcaster})
}

func (self *registry) endpoint(pid int) (string, error) {
	v, ok := self.syncMap.Load(pid)
	if !ok {
		return "", errors.New("no endpoint registered")
	}
	s := v.(scope)
	return s.endpoint, nil
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
