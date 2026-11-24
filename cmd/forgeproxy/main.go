package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"forgeproxy"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8088", "listen address")
	every := flag.Duration("every", 0, "health check interval; zero disables the loop")
	var specs routeFlags
	flag.Var(&specs, "route", "prefix=http://host,http://host")
	flag.Parse()
	if len(specs) == 0 {
		fmt.Fprintln(os.Stderr, "route is required")
		os.Exit(1)
	}
	proxy, err := forgeproxy.New(forgeproxy.Config{
		Routes:      []forgeproxy.Route(specs),
		HealthEvery: *every,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv, err := proxy.Listen(*listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer srv.Close()
	fmt.Printf("ForgeProxy listening on %s\n", srv.Addr())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}

type routeFlags []forgeproxy.Route

func (f *routeFlags) String() string { return "" }

func (f *routeFlags) Set(s string) error {
	route, err := forgeproxy.ParseRoute(s)
	if err != nil {
		return err
	}
	*f = append(*f, route)
	return nil
}
