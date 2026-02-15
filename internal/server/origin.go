package server

import "net/url"

type builtinOrigin struct {
	scheme  string
	host    string
	portAny bool
}

var builtinOrigins = []builtinOrigin{
	{scheme: "tauri", host: "localhost", portAny: false},
	{scheme: "https", host: "tauri.localhost", portAny: false},
	{scheme: "https", host: "tauri.local", portAny: true},
	{scheme: "http", host: "localhost", portAny: true},
	{scheme: "http", host: "127.0.0.1", portAny: true},
}

func isBuiltinOrigin(u *url.URL) bool {
	if u == nil {
		return false
	}
	hostname := u.Hostname()
	port := u.Port()
	for _, b := range builtinOrigins {
		if u.Scheme != b.scheme {
			continue
		}
		if hostname != b.host {
			continue
		}
		if !b.portAny && port != "" {
			continue
		}
		return true
	}
	return false
}
