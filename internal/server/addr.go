package server

import (
	"fmt"
	"net"
	"strings"
)

// DefaultAddr is the loopback listen address.
const DefaultAddr = "127.0.0.1:4477"

// ValidateAddr allows only loopback hosts: 127.0.0.0/8, ::1, or localhost.
func ValidateAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid addr %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing non-loopback address %q", addr)
	}
	return nil
}
