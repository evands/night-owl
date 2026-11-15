//go:build windows

package main

import "syscall"

// reuseControl lets this app share the DJ Link ports with other software
// on the same machine (rekordbox, Beat Link Trigger).
func reuseControl(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
