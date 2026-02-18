package validate

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// IdentRe matches valid identifiers used for namespaces, slugs, and plugin IDs.
// Must start with alphanumeric, followed by alphanumeric, dots, hyphens, or underscores.
var IdentRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// MaxIdentLen is the maximum length for identifiers (namespaces, slugs).
const MaxIdentLen = 128

// Ident validates a string as a valid identifier (namespace or slug).
func Ident(s string) bool {
	return len(s) > 0 && len(s) <= MaxIdentLen && IdentRe.MatchString(s)
}

// HTTPURL ensures the URL uses http or https scheme and has a non-empty host
// to prevent SSRF via file://, ftp://, or other dangerous schemes.
func HTTPURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		// OK
	case "":
		return fmt.Errorf("URL missing scheme: %s", rawURL)
	default:
		return fmt.Errorf("URL scheme %q not allowed (only http/https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL missing host: %s", rawURL)
	}
	return nil
}

// RejectPrivateURL checks whether the URL's host is a private or internal
// IP address (loopback, link-local, RFC-1918, or "localhost"). This blocks
// the most common SSRF vectors such as cloud metadata endpoints
// (169.254.169.254) and internal network probing.
//
// It only inspects literal IP addresses and the "localhost" hostname.
// DNS-resolved addresses are not checked here (that requires transport-level
// protection).
func RejectPrivateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return nil // Let HTTPURL handle empty host
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("URL host %q is a private/internal address", host)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil // Not an IP literal; DNS rebinding is a separate concern
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("URL host %q is a private/internal address", host)
	}
	return nil
}
