package workers

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

// listenTCP opens a listening socket for "IP:port". The socket is created
// with system calls (instead of net.Listen) so that nothing is read from
// /proc; the address must be an IP address.
func listenTCP(addr string) (*net.TCPListener, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("%s: invalid port", addr)
	}
	ip := net.ParseIP(host)
	if host == "" {
		ip = net.IPv4zero
	}
	if ip == nil {
		return nil, fmt.Errorf("%s: the listen address must be an IP address", addr)
	}
	var fd int
	if ip4 := ip.To4(); ip4 != nil {
		fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		sa := &syscall.SockaddrInet4{Port: port}
		copy(sa.Addr[:], ip4)
		err = bindListen(fd, sa)
	} else {
		fd, err = syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		sa := &syscall.SockaddrInet6{Port: port}
		copy(sa.Addr[:], ip.To16())
		err = bindListen(fd, sa)
	}
	if err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	f := os.NewFile(uintptr(fd), "listener "+addr)
	ln, err := net.FileListener(f)
	_ = f.Close() // FileListener keeps its own copy of the descriptor
	if err != nil {
		return nil, err
	}
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("%s: not a TCP listener", addr)
	}
	return tl, nil
}

func bindListen(fd int, sa syscall.Sockaddr) error {
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return err
	}
	if err := syscall.Bind(fd, sa); err != nil {
		return err
	}
	return syscall.Listen(fd, listenBacklog)
}

// listenBacklog is the queue length of pending connections.
const listenBacklog = 512
