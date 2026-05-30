package redis

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParseURL parses a Redis connection URL and returns the Options it
// implies. The shapes accepted are:
//
//	redis://[user:password@]host[:port][/db]
//	rediss://[user:password@]host[:port][/db]
//
// Only host is required. Defaults: port 6379, db 0, no auth, no TLS.
// The "rediss" scheme enables TLS with a config that asserts the
// remote ServerName matches the URL host — set Options.TLS afterwards
// for finer control (custom root CAs, mTLS).
//
// Anything else in the URL — query parameters, fragments, paths
// beyond the db segment — is ignored to keep the surface small and
// the parser predictable.
func ParseURL(rawurl string) (Options, error) {
	if rawurl == "" {
		return Options{}, errors.New("redis: empty URL")
	}
	u, err := url.Parse(rawurl)
	if err != nil {
		return Options{}, fmt.Errorf("redis: parse URL %q: %w", rawurl, err)
	}
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return Options{}, fmt.Errorf("redis: unsupported scheme %q (want redis or rediss)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return Options{}, fmt.Errorf("redis: URL %q has no host", rawurl)
	}
	port := u.Port()
	if port == "" {
		port = "6379"
	}

	opts := Options{Addr: host + ":" + port}

	if u.User != nil {
		opts.Username = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			opts.Password = pw
		}
	}

	// Path is "/<db>" if specified.
	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		db, err := strconv.Atoi(p)
		if err != nil {
			return Options{}, fmt.Errorf("redis: invalid db %q: %w", p, err)
		}
		opts.DB = db
	}

	if u.Scheme == "rediss" {
		// Minimal sensible default. Callers that need custom roots,
		// mTLS, or to disable verification (don't) can overwrite the
		// returned opts.TLS.
		opts.TLS = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	}
	return opts, nil
}
