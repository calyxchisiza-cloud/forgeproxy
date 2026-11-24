package forgeproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// ErrInvalid means the proxy configuration cannot be used.
var ErrInvalid = errors.New("proxy is not valid")

// Route sends a path prefix to one or more backends, in the order given.
// Prefix "/" matches every path. A longer prefix wins.
type Route struct {
	Prefix   string
	Backends []string
}

// Config is the routes and the health check. A zero HealthEvery leaves
// checks to Check. Backends start healthy.
type Config struct {
	Routes        []Route
	HealthPath    string
	HealthEvery   time.Duration
	DialTimeout   time.Duration
	HeaderTimeout time.Duration
}

// Proxy is an HTTP reverse proxy and a small load balancer.
type Proxy struct {
	routes   []route
	backends []*backend
	health   string
	every    time.Duration
	client   *http.Client
}

type route struct {
	prefix string
	backs  []*backend
	seq    atomic.Uint64
}

type backend struct {
	raw string
	u   *url.URL
	up  atomic.Bool
}

// New checks the routes and builds a proxy. Backends are shared by URL, so
// two routes that name the same origin share one health flag.
func New(cfg Config) (*Proxy, error) {
	if len(cfg.Routes) == 0 {
		return nil, fmt.Errorf("%w: route is required", ErrInvalid)
	}
	health := cfg.HealthPath
	if health == "" {
		health = "/health"
	}
	if !strings.HasPrefix(health, "/") {
		return nil, fmt.Errorf("%w: health path is not valid", ErrInvalid)
	}
	dial := cfg.DialTimeout
	if dial == 0 {
		dial = 2 * time.Second
	}
	header := cfg.HeaderTimeout
	if header == 0 {
		header = 5 * time.Second
	}
	if dial < 0 || header < 0 || cfg.HealthEvery < 0 {
		return nil, fmt.Errorf("%w: timeout is not valid", ErrInvalid)
	}
	p := &Proxy{
		health: health,
		every:  cfg.HealthEvery,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: dial}).DialContext,
				ResponseHeaderTimeout: header,
				IdleConnTimeout:       30 * time.Second,
				MaxIdleConns:          32,
				MaxIdleConnsPerHost:   4,
			},
		},
	}
	seenPrefix := map[string]bool{}
	pool := map[string]*backend{}
	for _, spec := range cfg.Routes {
		prefix, err := cleanPrefix(spec.Prefix)
		if err != nil {
			return nil, err
		}
		if seenPrefix[prefix] {
			return nil, fmt.Errorf("%w: duplicate prefix %s", ErrInvalid, prefix)
		}
		seenPrefix[prefix] = true
		if len(spec.Backends) == 0 {
			return nil, fmt.Errorf("%w: %s has no backend", ErrInvalid, prefix)
		}
		rt := route{prefix: prefix}
		seenBack := map[string]bool{}
		for _, raw := range spec.Backends {
			b, err := p.intern(pool, raw)
			if err != nil {
				return nil, err
			}
			if seenBack[b.raw] {
				return nil, fmt.Errorf("%w: duplicate backend %s", ErrInvalid, raw)
			}
			seenBack[b.raw] = true
			rt.backs = append(rt.backs, b)
		}
		p.routes = append(p.routes, rt)
	}
	return p, nil
}

func (p *Proxy) intern(pool map[string]*backend, raw string) (*backend, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%w: backend %s", ErrInvalid, raw)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("%w: backend %s has a path", ErrInvalid, raw)
	}
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	key := u.String()
	if b := pool[key]; b != nil {
		return b, nil
	}
	b := &backend{raw: key, u: u}
	b.up.Store(true)
	pool[key] = b
	p.backends = append(p.backends, b)
	return b, nil
}

func cleanPrefix(prefix string) (string, error) {
	if prefix == "" || prefix == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		return "", fmt.Errorf("%w: prefix %s", ErrInvalid, prefix)
	}
	if strings.Contains(prefix, " ") {
		return "", fmt.Errorf("%w: prefix %s", ErrInvalid, prefix)
	}
	return prefix, nil
}

// ParseRoute reads "prefix=http://host,http://host".
func ParseRoute(s string) (Route, error) {
	prefix, rest, ok := strings.Cut(s, "=")
	if !ok || prefix == "" || rest == "" {
		return Route{}, fmt.Errorf("%w: route %s", ErrInvalid, s)
	}
	backs := strings.Split(rest, ",")
	for _, b := range backs {
		if strings.TrimSpace(b) == "" {
			return Route{}, fmt.Errorf("%w: route %s", ErrInvalid, s)
		}
	}
	return Route{Prefix: prefix, Backends: backs}, nil
}

// Healthy reports the current flag for a backend URL.
func (p *Proxy) Healthy(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	key := u.String()
	for _, b := range p.backends {
		if b.raw == key {
			return b.up.Load()
		}
	}
	return false
}

// Check probes every backend. A 2xx answer marks it healthy.
func (p *Proxy) Check(ctx context.Context) {
	for _, b := range p.backends {
		if p.probe(ctx, b) {
			b.up.Store(true)
		} else {
			b.up.Store(false)
		}
	}
}

func (p *Proxy) probe(ctx context.Context, b *backend) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	u := *b.u
	u.Path = p.health
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (p *Proxy) loop(ctx context.Context) {
	p.Check(ctx)
	ticker := time.NewTicker(p.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Check(ctx)
		}
	}
}

// ServeHTTP forwards one request. No matching route is 404. No healthy
// backend is 503. A dial failure tries the next healthy backend when the
// body can be read again. Anything else that fails before a response is 502.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	rt := p.match(path)
	if rt == nil {
		http.Error(w, "no route", http.StatusNotFound)
		return
	}
	order := rt.order()
	if len(order) == 0 {
		http.Error(w, "no backend", http.StatusServiceUnavailable)
		return
	}
	for i, b := range order {
		if i > 0 && !replayable(r) {
			break
		}
		wrote, err := p.forward(w, r, b)
		if err == nil {
			return
		}
		if wrote || r.Context().Err() != nil {
			return
		}
		if dialFailed(err) {
			b.up.Store(false)
			continue
		}
		break
	}
	if r.Context().Err() != nil {
		return
	}
	http.Error(w, "bad gateway", http.StatusBadGateway)
}

func (p *Proxy) match(path string) *route {
	var best *route
	for i := range p.routes {
		rt := &p.routes[i]
		if !rt.matches(path) {
			continue
		}
		if best == nil || len(rt.prefix) > len(best.prefix) {
			best = rt
		}
	}
	return best
}

func (rt *route) matches(path string) bool {
	if rt.prefix == "/" {
		return true
	}
	return path == rt.prefix || strings.HasPrefix(path, rt.prefix+"/")
}

func (rt *route) order() []*backend {
	up := make([]*backend, 0, len(rt.backs))
	for _, b := range rt.backs {
		if b.up.Load() {
			up = append(up, b)
		}
	}
	if len(up) == 0 {
		return nil
	}
	start := int((rt.seq.Add(1) - 1) % uint64(len(up)))
	out := make([]*backend, len(up))
	for i := range up {
		out[i] = up[(start+i)%len(up)]
	}
	return out
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, b *backend) (bool, error) {
	body, err := requestBody(r)
	if err != nil {
		return false, err
	}
	target := *b.u
	target.Path = r.URL.Path
	if target.Path == "" {
		target.Path = "/"
	}
	target.RawPath = r.URL.RawPath
	target.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		return false, err
	}
	req.ContentLength = r.ContentLength
	req.Host = b.u.Host
	copyHeaders(req.Header, r.Header)
	ip := clientIP(r)
	if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
		req.Header.Set("X-Forwarded-For", prior+", "+ip)
	} else if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}
	if req.Header.Get("X-Forwarded-Host") == "" && r.Host != "" {
		req.Header.Set("X-Forwarded-Host", r.Host)
	}
	if req.Header.Get("X-Forwarded-Proto") == "" {
		if r.TLS == nil {
			req.Header.Set("X-Forwarded-Proto", "http")
		} else {
			req.Header.Set("X-Forwarded-Proto", "https")
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		return true, err
	}
	return true, nil
}

func requestBody(r *http.Request) (io.ReadCloser, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	if r.GetBody != nil {
		return r.GetBody()
	}
	return r.Body, nil
}

func replayable(r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return true
	}
	return r.GetBody != nil
}

func dialFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func copyHeaders(dst, src http.Header) {
	hop := hopHeaders(src)
	for key, values := range src {
		if hop[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

func hopHeaders(h http.Header) map[string]bool {
	hop := map[string]bool{
		"Connection":          true,
		"Keep-Alive":          true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"Te":                  true,
		"Trailer":             true,
		"Transfer-Encoding":   true,
		"Upgrade":             true,
	}
	for _, value := range h.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				hop[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	return hop
}

// Server accepts client connections for one proxy.
type Server struct {
	proxy  *Proxy
	http   *http.Server
	ln     net.Listener
	cancel context.CancelFunc
}

// Listen serves the proxy at addr. The returned server is already accepting.
func (p *Proxy) Listen(addr string) (*Server, error) {
	if addr == "" {
		return nil, errors.New("address is required")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		proxy:  p,
		ln:     ln,
		cancel: cancel,
		http: &http.Server{
			Handler:           p,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
	if p.every > 0 {
		go p.loop(ctx)
	}
	go s.http.Serve(ln)
	return s, nil
}

// Addr is the address clients can dial.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops accepting and stops health checks.
func (s *Server) Close() error {
	s.cancel()
	err := s.http.Close()
	s.proxy.client.CloseIdleConnections()
	return err
}
