# Architecture

A client connects to ForgeProxy. The proxy reads one HTTP request, chooses a route, chooses a backend, and opens its own connection to that origin. The client never sees the backend address.

Routes are path prefixes. `/api` matches `/api` and `/api/v1`, and it does not match `/apiv2`. `/` matches everything that a longer prefix did not take. The match is the longest prefix.

Each route keeps a counter. The next healthy backend in that route's list receives the request. The counter moves even when several clients arrive together, so the turns stay shared. A backend URL used by two routes has one health flag.

Health is a `GET` of `/health` on the origin. Success is a status from 200 to 299. `Check` runs that probe for every backend. When a dial fails during a real request, that backend is marked down immediately and the proxy tries the next healthy one. The retry happens only if the body is empty or `GetBody` can produce it again. A POST body that cannot be reread stops at 502 rather than running twice. A timeout after the request was written is not retried.

The upstream request keeps the method, path, query, and end-to-end headers. `Connection` and the headers it names are dropped, along with the other hop-by-hop names. The response is copied the same way, including its status. A 302 from the backend is the client's 302.

v1 listens for plain HTTP. A backend URL may be `http` or `https`. There is one process and no shared session between proxies.
