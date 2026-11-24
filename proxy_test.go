package forgeproxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRoundRobinAndPrefixes(t *testing.T) {
	apiA := textServer(t, "alpha")
	apiB := textServer(t, "beta")
	files := textServer(t, "static")
	root := textServer(t, "root")
	p := mustProxy(t, Config{Routes: []Route{
		{Prefix: "/api", Backends: []string{apiA.URL, apiB.URL}},
		{Prefix: "/files", Backends: []string{files.URL}},
		{Prefix: "/", Backends: []string{root.URL}},
	}})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	got := []string{
		get(t, srv.URL+"/api"),
		get(t, srv.URL+"/api/v1"),
		get(t, srv.URL+"/files/a"),
		get(t, srv.URL+"/apiv2"),
	}
	want := []string{"alpha", "beta", "static", "root"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q want %q", got, want)
		}
	}
	res, err := http.Get(srv.URL + "/missing-not-a-prefix")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("catch-all status %d", res.StatusCode)
	}
}

func TestFailoverSkipsAClosedBackend(t *testing.T) {
	dead := closedAddr(t)
	var hits atomic.Int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("live"))
	}))
	t.Cleanup(live.Close)
	p := mustProxy(t, Config{Routes: []Route{{
		Prefix:   "/",
		Backends: []string{"http://" + dead, live.URL},
	}}})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	if got := get(t, srv.URL+"/x"); got != "live" {
		t.Fatalf("got %q", got)
	}
	if p.Healthy("http://" + dead) {
		t.Fatal("closed backend still healthy")
	}
	if hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
}

func TestPostIsNotRetried(t *testing.T) {
	dead := closedAddr(t)
	var hits atomic.Int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(live.Close)
	p := mustProxy(t, Config{Routes: []Route{{
		Prefix:   "/",
		Backends: []string{"http://" + dead, live.URL},
	}}})
	body := io.NopCloser(strings.NewReader("hi"))
	req := httptest.NewRequest(http.MethodPost, "http://proxy/submit", body)
	req.GetBody = nil
	req.ContentLength = 2
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
	if hits.Load() != 0 {
		t.Fatalf("hits %d", hits.Load())
	}
}

func TestHealthDropsABackend(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("bad"))
	}))
	t.Cleanup(bad.Close)
	good := textServer(t, "good")
	p := mustProxy(t, Config{Routes: []Route{{
		Prefix:   "/",
		Backends: []string{bad.URL, good.URL},
	}}})
	p.Check(t.Context())
	if p.Healthy(bad.URL) {
		t.Fatal("bad health stayed up")
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	if got := get(t, srv.URL+"/work"); got != "good" {
		t.Fatalf("got %q", got)
	}
	if got := get(t, srv.URL+"/work"); got != "good" {
		t.Fatalf("second %q", got)
	}
}

func TestHeadersAndRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/go" {
			w.Header().Set("Location", "/next")
			w.Header().Set("Keep-Alive", "timeout=5")
			w.WriteHeader(http.StatusFound)
			return
		}
		if r.Header.Get("Keep-Alive") != "" {
			http.Error(w, "hop", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Seen-Host", r.Host)
		w.Header().Set("X-Seen-For", r.Header.Get("X-Forwarded-For"))
		w.Header().Set("X-Seen-Fwd-Host", r.Header.Get("X-Forwarded-Host"))
		w.Header().Set("X-Seen-Proto", r.Header.Get("X-Forwarded-Proto"))
		w.Write([]byte(r.RequestURI))
	}))
	t.Cleanup(upstream.Close)
	p := mustProxy(t, Config{Routes: []Route{{Prefix: "/", Backends: []string{upstream.URL}}}})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, res)
	if res.StatusCode != http.StatusOK || body != "/api?x=1" {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	if got := res.Header.Get("X-Seen-Host"); got != hostOf(upstream.URL) {
		t.Fatalf("host %q", got)
	}
	if got := res.Header.Get("X-Seen-For"); !strings.Contains(got, "10.0.0.1") {
		t.Fatalf("forwarded %q", got)
	}
	if got := res.Header.Get("X-Seen-Fwd-Host"); got == "" {
		t.Fatal("missing forwarded host")
	}
	if got := res.Header.Get("X-Seen-Proto"); got != "http" {
		t.Fatalf("proto %q", got)
	}

	res, err = client.Get(srv.URL + "/go")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/next" {
		t.Fatalf("redirect %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res.Header.Get("Keep-Alive") != "" {
		t.Fatalf("keep-alive %q", res.Header.Get("Keep-Alive"))
	}
}

func TestConcurrentSharesTheBackends(t *testing.T) {
	var a, b atomic.Int32
	left := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { a.Add(1) }))
	right := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { b.Add(1) }))
	t.Cleanup(left.Close)
	t.Cleanup(right.Close)
	p := mustProxy(t, Config{Routes: []Route{{
		Prefix:   "/",
		Backends: []string{left.URL, right.URL},
	}}})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := http.Get(srv.URL + "/")
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Errorf("status %d", res.StatusCode)
			}
		}()
	}
	wg.Wait()
	if a.Load()+b.Load() != 20 || a.Load() != 10 || b.Load() != 10 {
		t.Fatalf("a %d b %d", a.Load(), b.Load())
	}
}

func TestListenAndParseRoute(t *testing.T) {
	up := textServer(t, "ok")
	route, err := ParseRoute("/api=" + up.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := mustProxy(t, Config{Routes: []Route{route}})
	srv, err := p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if got := get(t, "http://"+srv.Addr()+"/api"); got != "ok" {
		t.Fatalf("got %q", got)
	}
	if _, err := ParseRoute("nope"); err == nil {
		t.Fatal("expected an invalid route")
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected a missing route")
	}
}

func TestBackendErrorStaysHealthy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(up.Close)
	p := mustProxy(t, Config{Routes: []Route{{Prefix: "/", Backends: []string{up.URL}}}})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/work", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	if !p.Healthy(up.URL) {
		t.Fatal("backend was marked down")
	}
}

func mustProxy(t *testing.T, cfg Config) *Proxy {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func textServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func get(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s status %d body %q", url, res.StatusCode, body)
	}
	return body
}

func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func hostOf(raw string) string {
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "https://")
	return raw
}
