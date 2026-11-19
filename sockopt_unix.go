//go:build !windows

package main

import (
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// reuseAddr lets several Loka processes listen on the discovery port.
func reuseAddr(fd uintptr) error {
	if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}
	return nil
}

func joinGroup(fd uintptr, group, local [4]byte) error {
	mreq := &syscall.IPMreq{Multiaddr: group, Interface: local}
	return syscall.SetsockoptIPMreq(int(fd), syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, mreq)
}

func multicastInterface(fd uintptr, local [4]byte) error {
	return syscall.SetsockoptInet4Addr(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, local)
}
