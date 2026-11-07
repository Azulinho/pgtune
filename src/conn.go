package main

import (
	"net/url"
	"strconv"
)

// buildDSN builds a libpq URL without hand-escaping values: url.UserPassword
// and url.Values encode userinfo and settings correctly. Host and port travel
// as query parameters so unix socket paths and IPv6 literals work.
func buildDSN(opts Options, db string) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(opts.User, opts.Password),
		Path:   "/" + db,
	}
	q := url.Values{}
	if opts.Host != "" {
		q.Set("host", opts.Host)
	}
	if opts.Port > 0 {
		q.Set("port", strconv.Itoa(opts.Port))
	}
	if opts.SslMode != "" {
		q.Set("sslmode", opts.SslMode)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
