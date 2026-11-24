# ForgeProxy

ForgeProxy accepts HTTP requests and forwards each one to a backend. A path prefix selects the route. The longest matching prefix wins. `/` matches the paths no longer prefix claimed. Backends on a route take turns. A backend that is not healthy is left out of the turn.

Backends start healthy. `Check` requests `GET /health` on each origin. A 2xx answer marks it healthy. Any other result, including a refused connection, marks it down. `HealthEvery` repeats that check. A request that cannot dial a healthy backend marks that backend down and tries the next one when the body can be read again. A body without `GetBody` is not sent twice. A response from a backend, including a 500, is returned as it arrived. Redirects are not followed.

Hop-by-hop headers are removed. `X-Forwarded-For` gains the client address. `X-Forwarded-Host` and `X-Forwarded-Proto` record the request the proxy received when the client did not already set them. The `Host` sent upstream is the backend host.

The proxy speaks ordinary HTTP. It can sit in front of ForgeHTTP or any other HTTP server. It does not import that module.

```go
proxy, err := forgeproxy.New(forgeproxy.Config{Routes: []forgeproxy.Route{
    {Prefix: "/api", Backends: []string{"http://127.0.0.1:9001", "http://127.0.0.1:9002"}},
    {Prefix: "/", Backends: []string{"http://127.0.0.1:9000"}},
}})
server, err := proxy.Listen("127.0.0.1:8088")
```

## Run

```
go run ./cmd/demo
```

The demo prints:

```
/api alpha
/api beta
/files static
```

The first `/api` request reaches alpha. The next reaches beta. `/files` has its own backend.

```
go run ./cmd/forgeproxy -listen 127.0.0.1:8088 -every 2s -route /api=http://127.0.0.1:9001,http://127.0.0.1:9002 -route /=http://127.0.0.1:9000
```

The listener stays on loopback until interrupt. It does not bind port 8080.

## Test

```
go test ./...
go test -race -count=1 ./...
```
