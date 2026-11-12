package server

import (
	"net"
	"sync"
)

// swappingListener serves Accept from a listener that can be replaced.
type swappingListener struct {
	mu     sync.Mutex
	ln     net.Listener
	closed bool
}

func newSwappingListener(ln net.Listener) *swappingListener {
	return &swappingListener{ln: ln}
}

func (s *swappingListener) Accept() (net.Conn, error) {
	for {
		s.mu.Lock()
		ln := s.ln
		closed := s.closed
		s.mu.Unlock()
		if closed || ln == nil {
			return nil, net.ErrClosed
		}
		conn, err := ln.Accept()
		if err == nil {
			return conn, nil
		}
		s.mu.Lock()
		replaced := s.ln != ln
		closed = s.closed
		s.mu.Unlock()
		if replaced && !closed {
			continue
		}
		return nil, err
	}
}

func (s *swappingListener) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.ln == nil {
		return nil
	}
	err := s.ln.Close()
	s.ln = nil
	return err
}

func (s *swappingListener) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return closedAddr{}
	}
	return s.ln.Addr()
}

func (s *swappingListener) Swap(ln net.Listener) {
	s.mu.Lock()
	old := s.ln
	s.ln = ln
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (s *swappingListener) current() net.Listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ln
}

type closedAddr struct{}

func (closedAddr) Network() string { return "tcp" }
func (closedAddr) String() string  { return "" }
