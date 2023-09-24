package server

import (
	"net"
)

type Listener interface {
	net.Listener
	Port() int
}

type listener struct {
	net.Listener
	port int
}

func NewListener() (Listener, error) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	return &listener{
		Listener: l,
		port:     l.Addr().(*net.TCPAddr).Port,
	}, nil
}

func (l *listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (l *listener) Port() int {
	return l.port
}
