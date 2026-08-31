package peers

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// normalizePeerURL validates raw as an absolute http(s) URL with no path,
// query, or fragment, and returns it as scheme://host[:port].
func normalizePeerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("must not have a path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must not have a query or fragment")
	}
	return u.Scheme + "://" + u.Host, nil
}

// parseDNSSpec parses the part of a "dns:" spec after the prefix into a
// host and a port. It rejects a missing host and a non-numeric or
// out-of-range port; it does not resolve the host.
func parseDNSSpec(rest string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(rest)
	if err != nil {
		return "", "", fmt.Errorf("peers: invalid dns spec %q: %w", rest, err)
	}
	if host == "" {
		return "", "", fmt.Errorf("peers: dns spec %q is missing a host", rest)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", "", fmt.Errorf("peers: dns spec %q has an invalid port %q", rest, port)
	}
	return host, port, nil
}
