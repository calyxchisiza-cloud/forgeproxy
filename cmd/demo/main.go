package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"forgeproxy"
)

func main() {
	alpha := start("alpha")
	defer alpha.Close()
	beta := start("beta")
	defer beta.Close()
	files := start("static")
	defer files.Close()
	proxy, err := forgeproxy.New(forgeproxy.Config{Routes: []forgeproxy.Route{
		{Prefix: "/api", Backends: []string{alpha.url, beta.url}},
		{Prefix: "/files", Backends: []string{files.url}},
	}})
	if err != nil {
		fail(err)
	}
	srv, err := proxy.Listen("127.0.0.1:0")
	if err != nil {
		fail(err)
	}
	defer srv.Close()
	for _, path := range []string{"/api", "/api", "/files"} {
		res, err := http.Get("http://" + srv.Addr() + path)
		if err != nil {
			fail(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			fail(err)
		}
		if res.StatusCode != http.StatusOK {
			fail(fmt.Errorf("%s status %d", path, res.StatusCode))
		}
		fmt.Printf("%s %s\n", path, body)
	}
}

type backend struct {
	url string
	srv *http.Server
}

func start(body string) *backend {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})}
	go srv.Serve(ln)
	return &backend{url: "http://" + ln.Addr().String(), srv: srv}
}

func (b *backend) Close() error { return b.srv.Close() }

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
