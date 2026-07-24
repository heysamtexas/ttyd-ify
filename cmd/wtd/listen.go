package main

import (
	"fmt"
	"net"
)

// Bind-address validation.
//
// This exists because the whole security model of ttyd-ify is "an unauthenticated,
// writable shell, protected only by the interface it is bound to". A wildcard bind
// silently converts that into a public shell, so wtd refuses one even when explicitly
// asked — the bash launcher (bin/wt-bind.sh) is expected to resolve a concrete address,
// and this is the belt-and-braces check for when something upstream goes wrong.
//
// Deliberately NOT a warning: bin/wt-serve exits non-zero when it cannot resolve a bind
// target, and this keeps that contract intact for the Go path.

// validateListen checks a -listen value of the form "host:port" and returns it
// normalized. It rejects any address that would listen on every interface.
func validateListen(addr string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("-listen is required (e.g. -listen 127.0.0.1:7681)")
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("-listen %q is not host:port: %w", addr, err)
	}
	if port == "" {
		return "", fmt.Errorf("-listen %q has no port", addr)
	}

	// An empty host is how Go spells "all interfaces" (":7681"), so it is a wildcard
	// even though it contains no address at all.
	if host == "" {
		return "", wildcardError(addr, `no host ("`+addr+`" listens on every interface)`)
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname rather than a literal. Refusing outright would break "localhost",
		// so resolve it and check every address it yields — a name can point at
		// 0.0.0.0 just as easily as a literal can.
		addrs, err := net.LookupHost(host)
		if err != nil {
			return "", fmt.Errorf("-listen %q: cannot resolve host %q: %w", addr, host, err)
		}
		for _, a := range addrs {
			if resolved := net.ParseIP(a); resolved != nil && resolved.IsUnspecified() {
				return "", wildcardError(addr, fmt.Sprintf("host %q resolves to %s", host, a))
			}
		}
		return addr, nil
	}

	if ip.IsUnspecified() { // 0.0.0.0 and ::
		return "", wildcardError(addr, fmt.Sprintf("%s is the unspecified address", ip))
	}

	return addr, nil
}

func wildcardError(addr, why string) error {
	return fmt.Errorf(`refusing to bind %q: %s.

  wtd serves an unauthenticated, writable shell. Binding every interface would
  expose it to every network this host is on, including the public internet.

  Bind a specific address instead — a tailnet IP, 127.0.0.1, or an interface's
  address. bin/wt-web-serve resolves one from WT_BIND in /etc/ttyd-ify/config.`, addr, why)
}
