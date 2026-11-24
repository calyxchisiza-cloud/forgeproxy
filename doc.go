// Package forgeproxy forwards HTTP requests to backends.
// A route picks a set of backends by path prefix. Healthy backends share the
// requests in turn. A backend that refuses the connection is skipped, and a
// health check can put it back. The proxy does not merge responses and it
// does not follow redirects.
package forgeproxy
