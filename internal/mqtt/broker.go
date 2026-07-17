package mqtt

import (
	"fmt"
	"strings"
)

// defaultMQTTPort is the plaintext MQTT port used when the broker address omits
// one. defaultMQTTSPort is used instead when the scheme resolves to TLS (ssl).
const (
	defaultMQTTPort  = "1883"
	defaultMQTTSPort = "8883"
)

// normalizeBrokerURL turns a loosely-typed broker address into the canonical
// "scheme://host:port" form paho expects, and returns a human-readable note
// describing any transformation it applied (empty when the input was already
// canonical). It is intentionally forgiving of the ways a user might paste a
// broker address — a bare host, a host:port pair, or even a browser URL copied
// out of the address bar (http://ha.local:8123).
//
// Recognized transformations:
//   - surrounding whitespace is trimmed and a single trailing slash removed
//   - the scheme is lowercased
//   - a missing scheme defaults to tcp://
//   - http:// / https:// / mqtt:// resolve to tcp://; mqtts:// / tls:// resolve
//     to ssl://; tcp/ssl/ws/wss are kept as-is
//   - a missing port defaults to 1883 (or 8883 when the scheme resolves to ssl)
//   - IPv6 literals are kept/placed in brackets ([::1]:1883)
//
// It never panics: if the address cannot be parsed it is returned unchanged with
// a note explaining that it was left as-is, so a malformed value degrades to the
// previous behavior rather than crashing startup.
func normalizeBrokerURL(raw string) (normalized string, note string) {
	s := strings.TrimSpace(raw)
	var notes []string
	if s != raw {
		notes = append(notes, "trimmed surrounding whitespace")
	}
	if s == "" {
		return raw, "empty broker address; left unchanged"
	}
	if strings.HasSuffix(s, "/") {
		s = strings.TrimSuffix(s, "/")
		notes = append(notes, "removed trailing slash")
	}

	// Split an optional "scheme://" prefix off the authority.
	rawScheme, authority := splitScheme(s)

	// Resolve the scheme. An empty rawScheme means none was supplied.
	scheme, schemeNote, ok := resolveScheme(rawScheme)
	if !ok {
		return raw, fmt.Sprintf("unrecognized scheme %q; left unchanged", rawScheme)
	}
	if schemeNote != "" {
		notes = append(notes, schemeNote)
	}

	host, port, bracketsAdded := splitAuthority(authority)
	if host == "" {
		return raw, fmt.Sprintf("could not parse broker address %q; left unchanged", raw)
	}
	if bracketsAdded {
		notes = append(notes, "added brackets around IPv6 literal")
	}
	if port == "" {
		port = defaultMQTTPort
		if scheme == "ssl" {
			port = defaultMQTTSPort
		}
		notes = append(notes, fmt.Sprintf("defaulted to port %s", port))
	}

	normalized = scheme + "://" + host + ":" + port
	return normalized, strings.Join(notes, "; ")
}

// hostPortIsHAWebUI reports whether a normalized broker address resolves to port
// 8123 — Home Assistant's web UI port, which users commonly paste by mistake in
// place of the MQTT broker's port (usually 1883).
func hostPortIsHAWebUI(normalized string) bool {
	_, authority := splitScheme(normalized)
	_, port, _ := splitAuthority(authority)
	return port == "8123"
}

// splitScheme separates an optional "scheme://" prefix from the authority.
// scheme is "" when the input carries no "://".
func splitScheme(s string) (scheme, authority string) {
	if i := strings.Index(s, "://"); i >= 0 {
		return s[:i], s[i+len("://"):]
	}
	return "", s
}

// resolveScheme maps a raw (as-typed) scheme to the canonical paho scheme and a
// note describing the mapping. ok is false for a scheme we do not recognize, so
// the caller can leave the address untouched rather than guess.
func resolveScheme(rawScheme string) (scheme, note string, ok bool) {
	if rawScheme == "" {
		return "tcp", "assumed tcp:// scheme", true
	}
	lower := strings.ToLower(rawScheme)
	switch lower {
	case "tcp", "ssl", "ws", "wss":
		if rawScheme != lower {
			return lower, "lowercased scheme to " + lower + "://", true
		}
		return lower, "", true
	case "http", "https":
		return "tcp", fmt.Sprintf("treated %s:// as tcp:// (that looks like a web address, not an MQTT broker)", lower), true
	case "mqtt":
		return "tcp", "converted mqtt:// to tcp://", true
	case "mqtts":
		return "ssl", "converted mqtts:// to ssl://", true
	case "tls":
		return "ssl", "converted tls:// to ssl://", true
	default:
		return "", "", false
	}
}

// splitAuthority splits "host", "host:port", "[ipv6]", or "[ipv6]:port" into its
// host and port parts. A bare IPv6 literal (more than one colon, no brackets) is
// wrapped in brackets and bracketsAdded is set. host is "" when the input is
// unparseable.
func splitAuthority(authority string) (host, port string, bracketsAdded bool) {
	if authority == "" {
		return "", "", false
	}
	if strings.HasPrefix(authority, "[") {
		end := strings.Index(authority, "]")
		if end < 0 {
			// Malformed bracketed literal; hand it back as-is for the caller to reject.
			return "", "", false
		}
		host = authority[:end+1]
		rest := authority[end+1:]
		if strings.HasPrefix(rest, ":") {
			port = rest[1:]
		}
		return host, port, false
	}
	// A bare IPv6 literal has multiple colons and no port we can distinguish.
	if strings.Count(authority, ":") > 1 {
		return "[" + authority + "]", "", true
	}
	if i := strings.LastIndex(authority, ":"); i >= 0 {
		return authority[:i], authority[i+1:], false
	}
	return authority, "", false
}
