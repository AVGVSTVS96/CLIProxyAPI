//go:build !unix

package handoff

import (
	"errors"
	"net"
	"os"
	"time"
)

var errUnsupported = errors.New("handoff needs a unix system")

func Requests() <-chan os.Signal               { return nil }
func Inherited() bool                          { return false }
func Listener() (net.Listener, error)          { return nil, errUnsupported }
func Complete(func()) error                    { return errUnsupported }
func Begin(*os.File) (*Successor, error)       { return nil, errUnsupported }
func (s *Successor) Ready(time.Duration) error { return errUnsupported }
func (s *Successor) Commit() error             { return errUnsupported }
func (s *Successor) Abort()                    {}

type Successor struct{}
