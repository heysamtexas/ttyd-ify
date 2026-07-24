package main

import "testing"

// The wildcard cases are the reason this file exists — everything else is incidental.
// If one of them ever starts passing validation, an unauthenticated shell becomes
// reachable from every network the host is attached to.
func TestValidateListenRejectsWildcards(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:7681",
		"[::]:7681",
		":7681",     // empty host: Go's spelling of "all interfaces"
		"0.0.0.0:0", // random port is still every interface
	} {
		if _, err := validateListen(addr); err == nil {
			t.Errorf("validateListen(%q) = nil error, want refusal", addr)
		}
	}
}

func TestValidateListenAcceptsConcreteAddresses(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:7681",
		"100.64.0.3:7681", // tailnet address, the production shape
		"[::1]:7681",
		"localhost:7681", // resolves to loopback only
	} {
		got, err := validateListen(addr)
		if err != nil {
			t.Errorf("validateListen(%q) = %v, want accepted", addr, err)
			continue
		}
		if got != addr {
			t.Errorf("validateListen(%q) = %q, want it returned unchanged", addr, got)
		}
	}
}

func TestValidateListenRejectsMalformed(t *testing.T) {
	for _, addr := range []string{
		"",
		"7681",            // no host:port separator
		"127.0.0.1",       // no port
		"127.0.0.1:",      // empty port
		"not a host:7681", // unresolvable
	} {
		if _, err := validateListen(addr); err == nil {
			t.Errorf("validateListen(%q) = nil error, want rejection", addr)
		}
	}
}
