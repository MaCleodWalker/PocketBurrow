package burrow

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token-at-least-thirty-two-characters"

type runtimeFunc func(context.Context, string) (string, error)

func (f runtimeFunc) Ensure(ctx context.Context, id string) (string, error) { return f(ctx, id) }

func testServer(t *testing.T, runtime Runtime) *Server {
	t.Helper()
	s, err := NewServer(testRegistry(t), runtime, "localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func adminRequest(s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Admin().ServeHTTP(w, req)
	return w
}

func TestAdminCreateValidateAndRetry(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer backend.Close()
	calls := 0
	fail := false
	s := testServer(t, runtimeFunc(func(_ context.Context, id string) (string, error) {
		calls++
		if fail {
			return "", errors.New("Docker unavailable")
		}
		return backend.URL, nil
	}))
	for _, token := range []string{"", "wrong"} {
		w := adminRequest(s, "POST", "/v1/tenants", `{"id":"acme"}`, token)
		if w.Code != 401 {
			t.Fatalf("unauthorized status %d", w.Code)
		}
	}
	for _, body := range []string{`{"id":"../escape"}`, `{"id":"UPPER"}`, `{"id":"a.b"}`,
		`{"id":"-bad"}`, `{"id":"bad-"}`, `{"id":"` + strings.Repeat("a", 64) + `"}`,
		`{"id":"acme","endpoint":"http://evil"}`, `{"id":"acme"} {}`, `{`} {
		w := adminRequest(s, "POST", "/v1/tenants", body, testToken)
		if w.Code != 400 {
			t.Errorf("body %s: status %d", body, w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("invalid or unauthorized input invoked runtime")
	}
	w := adminRequest(s, "POST", "/v1/tenants", `{"id":"acme"}`, testToken)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var tenant Tenant
	if err := json.Unmarshal(w.Body.Bytes(), &tenant); err != nil {
		t.Fatal(err)
	}
	if tenant.Host != "acme.localhost" || tenant.State != "ready" || tenant.Endpoint != backend.URL {
		t.Fatalf("created tenant %+v", tenant)
	}
	if w := adminRequest(s, "POST", "/v1/tenants", `{"id":"acme"}`, testToken); w.Code != 409 {
		t.Fatalf("duplicate status %d", w.Code)
	}
	fail = true
	w = adminRequest(s, "POST", "/v1/tenants", `{"id":"beta"}`, testToken)
	if w.Code != 502 {
		t.Fatalf("failure status %d", w.Code)
	}
	tenant, _ = s.registry.Get("beta")
	if tenant.State != "error" || !strings.Contains(tenant.Error, "Docker unavailable") {
		t.Fatalf("failure not persisted: %+v", tenant)
	}
	fail = false
	w = adminRequest(s, "POST", "/v1/tenants/beta/start", "", testToken)
	if w.Code != 200 {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	if w := adminRequest(s, "GET", "/v1/tenants/acme/health", "", testToken); w.Code != 200 {
		t.Fatalf("health status %d", w.Code)
	}
	if w := adminRequest(s, "POST", "/v1/tenants/missing/start", "", testToken); w.Code != 404 {
		t.Fatalf("missing status %d", w.Code)
	}
	w = adminRequest(s, "GET", "/v1/tenants", "", testToken)
	var tenants []Tenant
	if err := json.Unmarshal(w.Body.Bytes(), &tenants); err != nil {
		t.Fatal(err)
	}
	if len(tenants) != 2 || tenants[0].ID != "acme" || tenants[1].ID != "beta" {
		t.Fatalf("list %+v", tenants)
	}
}

func TestGatewayIsolationAndForwardedHeaders(t *testing.T) {
	s := testServer(t, nil)
	for _, id := range []string{"acme", "beta"} {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tenant-auth" {
				t.Error("tenant authorization not preserved")
			}
			if r.Header.Get("X-Forwarded-For") != "192.0.2.1" ||
				r.Header.Get("X-Forwarded-Host") != r.Host ||
				r.Header.Get("X-Forwarded-Proto") != "http" ||
				r.Header.Get("Forwarded") != "" || r.Header.Get("X-Real-IP") != "" {
				t.Errorf("unsafe forwarded headers: %v", r.Header)
			}
			if r.URL.RawQuery != "filter=test" || r.URL.Path != "/api/records" {
				t.Errorf("request URL not preserved: %s", r.URL)
			}
			io.WriteString(w, id)
		}))
		defer backend.Close()
		if err := s.registry.Put(Tenant{ID: id, Host: id + ".localhost", State: "ready", Endpoint: backend.URL}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		host string
		code int
		body string
	}{
		{"acme.localhost:8080", 200, "acme"}, {"BETA.LOCALHOST.", 200, "beta"},
		{"unknown.localhost", 404, ""}, {"acme.localhost.evil", 404, ""},
		{"nested.acme.localhost", 404, ""}, {"localhost", 404, ""}, {"acme.localhost:bad:host", 400, ""},
	} {
		r := httptest.NewRequest("GET", "/api/records?filter=test", nil)
		r.Host = tc.host
		r.Header.Set("Authorization", "Bearer tenant-auth")
		for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-IP"} {
			r.Header.Set(header, "spoofed")
		}
		w := httptest.NewRecorder()
		s.Gateway().ServeHTTP(w, r)
		if w.Code != tc.code || (tc.body != "" && w.Body.String() != tc.body) {
			t.Errorf("host %s: %d %s", tc.host, w.Code, w.Body.String())
		}
	}
}

func TestGatewayStreamsRealtime(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: connected\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()
	s := testServer(t, nil)
	if err := s.registry.Put(Tenant{ID: "acme", Host: "acme.localhost", State: "ready", Endpoint: backend.URL}); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(s.Gateway())
	defer gateway.Close()
	req, err := http.NewRequest("GET", gateway.URL+"/api/realtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "acme.localhost"
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer res.Body.Close()
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	close(release)
	if err != nil || line != "data: connected\n" {
		t.Fatalf("stream did not flush before upstream completed: %q %v", line, err)
	}
}

func TestGatewayUnavailable(t *testing.T) {
	s := testServer(t, nil)
	if err := s.registry.Put(Tenant{ID: "acme", Host: "acme.localhost", State: "error"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://acme.localhost/api/health", nil)
	w := httptest.NewRecorder()
	s.Gateway().ServeHTTP(w, req)
	if w.Code != 503 {
		t.Fatalf("not ready status %d", w.Code)
	}
	backend := httptest.NewServer(http.NotFoundHandler())
	endpoint := backend.URL
	backend.Close()
	if err := s.registry.Put(Tenant{ID: "acme", Host: "acme.localhost", State: "ready", Endpoint: endpoint}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.Gateway().ServeHTTP(w, req)
	if w.Code != 502 {
		t.Fatalf("unreachable status %d", w.Code)
	}
}

func TestReconcileAndConfiguration(t *testing.T) {
	for _, domain := range []string{"", "127.0.0.1", "bad:8080", "*.localhost", "localhost/path"} {
		if _, err := NewServer(testRegistry(t), nil, domain, testToken); err == nil {
			t.Errorf("invalid domain accepted %q", domain)
		}
	}
	if _, err := NewServer(testRegistry(t), nil, "localhost", "short"); err == nil {
		t.Fatal("short token accepted")
	}
	for _, endpoint := range []string{"http://evil:8090", "https://127.0.0.1:8090", "http://127.0.0.1",
		"http://user:pass@127.0.0.1:8090", "http://127.0.0.1:8090/path", "http://127.0.0.1:70000",
		"http://127.0.0.1:0", "http://127.0.0.1:8090?"} {
		if _, err := upstreamURL(endpoint); err == nil {
			t.Errorf("invalid upstream accepted %q", endpoint)
		}
	}

	s := testServer(t, runtimeFunc(func(_ context.Context, id string) (string, error) {
		if id != "acme" {
			t.Errorf("reconciled non-ready tenant %s", id)
		}
		return "http://127.0.0.1:45678", nil
	}))
	for _, tenant := range []Tenant{
		{ID: "acme", Host: "acme.localhost", State: "ready", Endpoint: "http://127.0.0.1:12345"},
		{ID: "beta", Host: "beta.localhost", State: "provisioning"},
	} {
		if err := s.registry.Put(tenant); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	tenant, _ := s.registry.Get("acme")
	if tenant.Endpoint != "http://127.0.0.1:45678" {
		t.Fatal("stale endpoint was not reconciled")
	}
	if _, err := NewServer(s.registry, nil, "example.com", testToken); err == nil {
		t.Fatal("changing base domain silently accepted")
	}
}

func TestProvisioningFailsBeforeRuntimeWhenPersistenceFails(t *testing.T) {
	s := testServer(t, runtimeFunc(func(_ context.Context, _ string) (string, error) {
		t.Fatal("runtime invoked without durable intent")
		return "", nil
	}))
	s.registry.path = filepath.Join(t.TempDir(), "missing-directory", "tenants.json")
	w := adminRequest(s, "POST", "/v1/tenants", `{"id":"acme"}`, testToken)
	if w.Code != 500 {
		t.Fatalf("persistence failure status %d", w.Code)
	}
	if _, exists := s.registry.Get("acme"); exists {
		t.Fatal("failed registry write published a tenant")
	}
}

func TestReconcileReportsRuntimeFailure(t *testing.T) {
	s := testServer(t, runtimeFunc(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("Docker unavailable")
	}))
	tenant := Tenant{ID: "acme", Host: "acme.localhost", State: "ready", Endpoint: "http://127.0.0.1:12345"}
	if err := s.registry.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(context.Background()); err == nil {
		t.Fatal("reconciliation hid Docker failure")
	}
	got, _ := s.registry.Get("acme")
	if got != tenant {
		t.Fatal("failed reconciliation changed placement")
	}
}
