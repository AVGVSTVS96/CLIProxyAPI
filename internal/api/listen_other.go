//go:build !unix

package api

import "net"

func listen(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
