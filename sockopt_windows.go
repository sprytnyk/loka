package main

import "syscall"

// reuseAddr lets several Loka processes listen on the discovery port.
func reuseAddr(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}

func joinGroup(fd uintptr, group, local [4]byte) error {
	mreq := &syscall.IPMreq{Multiaddr: group, Interface: local}
	return syscall.SetsockoptIPMreq(syscall.Handle(fd), syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, mreq)
}

func multicastInterface(fd uintptr, local [4]byte) error {
	return syscall.SetsockoptInet4Addr(syscall.Handle(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, local)
}
