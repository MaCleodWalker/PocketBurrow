package burrow

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	registry *Registry
	runtime  Runtime
	domain   string
	token    [32]byte
	ops      sync.Mutex
	health   *http.Client
}

func NewServer(r *Registry, runtime Runtime, domain, token string) (*Server, error) {
	if len(token) < 32 {
		return nil, errors.New("admin token must be at least 32 characters")
	}
	domain = strings.ToLower(domain)
	if len(domain) > 189 || strings.Contains(domain, ":") || net.ParseIP(domain) != nil {
		return nil, errors.New("base domain must be a DNS name, not an IP address")
	}
	for _, label := range strings.Split(domain, ".") {
		if !tenantID.MatchString(label) {
			return nil, errors.New("invalid base domain")
		}
	}
	for _, t := range r.List() {
		if t.Host != t.ID+"."+domain {
			return nil, fmt.Errorf("tenant %s belongs to another base domain", t.ID)
		}
		if t.State == "ready" {
			if _, err := upstreamURL(t.Endpoint); err != nil {
				return nil, fmt.Errorf("tenant %s: %w", t.ID, err)
			}
		}
	}
	return &Server{registry: r, runtime: runtime, domain: domain, token: sha256.Sum256([]byte(token)),
		health: &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}}}, nil
}

func upstreamURL(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || port == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("upstream must be a loopback HTTP endpoint")
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return nil, errors.New("invalid upstream port")
	}
	return u, nil
}

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write response", "error", err)
	}
}

func failure(w http.ResponseWriter, status int, err error) {
	if status >= 500 {
		slog.Error("request failed", "status", status, "error", err)
	}
	respond(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) Admin() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/tenants", func(w http.ResponseWriter, _ *http.Request) {
		tenants := s.registry.List()
		sort.Slice(tenants, func(i, j int) bool { return tenants[i].ID < tenants[j].ID })
		respond(w, 200, tenants)
	})
	mux.HandleFunc("POST /v1/tenants", s.create)
	mux.HandleFunc("POST /v1/tenants/{id}/start", s.start)
	mux.HandleFunc("GET /v1/tenants/{id}/health", s.tenantHealth)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			provided := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") ||
				subtle.ConstantTimeCompare(provided[:], s.token[:]) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				failure(w, 401, errors.New("unauthorized"))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID string `json:"id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		failure(w, 400, fmt.Errorf("invalid request: %w", err))
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		failure(w, 400, errors.New("expected exactly one JSON object"))
		return
	}
	if !tenantID.MatchString(input.ID) {
		failure(w, 400, errors.New("id must be a lowercase DNS label of 1-63 characters"))
		return
	}
	s.ops.Lock()
	defer s.ops.Unlock()
	if _, exists := s.registry.Get(input.ID); exists {
		failure(w, 409, errors.New("tenant already exists; use its start endpoint to retry provisioning"))
		return
	}
	t := Tenant{ID: input.ID, Host: input.ID + "." + s.domain, State: "provisioning", CreatedAt: time.Now().UTC()}
	if err := s.registry.Put(t); err != nil {
		failure(w, 500, err)
		return
	}
	s.provision(w, r, t, http.StatusCreated)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	s.ops.Lock()
	defer s.ops.Unlock()
	t, ok := s.registry.Get(r.PathValue("id"))
	if !ok {
		failure(w, 404, errors.New("tenant not found"))
		return
	}
	t.State, t.Endpoint, t.Error = "provisioning", "", ""
	if err := s.registry.Put(t); err != nil {
		failure(w, 500, err)
		return
	}
	s.provision(w, r, t, 200)
}

func (s *Server) provision(w http.ResponseWriter, r *http.Request, t Tenant, status int) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	endpoint, err := s.runtime.Ensure(ctx, t.ID)
	if err == nil {
		_, err = upstreamURL(endpoint)
	}
	if err == nil {
		err = s.waitHealthy(ctx, endpoint)
	}
	if err != nil {
		t.State, t.Error = "error", err.Error()
		if saveErr := s.registry.Put(t); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("save failed provisioning state: %w", saveErr))
		}
		failure(w, 502, err)
		return
	}
	t.State, t.Endpoint, t.Error = "ready", endpoint, ""
	if err := s.registry.Put(t); err != nil {
		failure(w, 500, err)
		return
	}
	respond(w, status, t)
}

func (s *Server) checkHealth(ctx context.Context, endpoint string) error {
	if _, err := upstreamURL(endpoint); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/health", nil)
	if err != nil {
		return err
	}
	res, err := s.health.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("PocketBase health returned HTTP %d", res.StatusCode)
	}
	return nil
}

func (s *Server) waitHealthy(ctx context.Context, endpoint string) error {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := s.checkHealth(ctx, endpoint)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("PocketBase did not become healthy: %w", err)
		case <-ticker.C:
		}
	}
}

func (s *Server) tenantHealth(w http.ResponseWriter, r *http.Request) {
	t, ok := s.registry.Get(r.PathValue("id"))
	if !ok {
		failure(w, 404, errors.New("tenant not found"))
		return
	}
	if t.State != "ready" {
		failure(w, 503, fmt.Errorf("tenant is %s", t.State))
		return
	}
	if err := s.checkHealth(r.Context(), t.Endpoint); err != nil {
		failure(w, 503, err)
		return
	}
	respond(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) Gateway() http.Handler {
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns: 100, MaxIdleConnsPerHost: 10, IdleConnTimeout: 90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if strings.Contains(host, ":") {
			var err error
			host, _, err = net.SplitHostPort(host)
			if err != nil {
				failure(w, 400, errors.New("invalid host"))
				return
			}
		}
		host = strings.TrimSuffix(host, ".")
		id, ok := strings.CutSuffix(host, "."+s.domain)
		t, exists := s.registry.Get(id)
		if !ok || !exists || t.Host != host {
			failure(w, 404, errors.New("unknown tenant host"))
			return
		}
		if t.State != "ready" {
			failure(w, 503, errors.New("tenant is not ready"))
			return
		}
		target, err := upstreamURL(t.Endpoint)
		if err != nil {
			failure(w, 502, err)
			return
		}
		proxy := &httputil.ReverseProxy{
			Transport: transport, FlushInterval: -1,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = pr.In.Host
				pr.Out.Header.Del("X-Real-IP")
				pr.SetXForwarded()
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				slog.Error("tenant proxy failed", "tenant", t.ID, "error", err)
				failure(w, 502, errors.New("tenant upstream unavailable"))
			},
		}
		proxy.ServeHTTP(w, r)
	})
}

func (s *Server) Reconcile(ctx context.Context) error {
	s.ops.Lock()
	defer s.ops.Unlock()
	for _, t := range s.registry.List() {
		// Interrupted and failed provisioning requires an explicit start retry.
		if t.State != "ready" {
			continue
		}
		endpoint, err := s.runtime.Ensure(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("reconcile tenant %s: %w", t.ID, err)
		}
		if _, err := upstreamURL(endpoint); err != nil {
			return fmt.Errorf("reconcile tenant %s: %w", t.ID, err)
		}
		t.Endpoint = endpoint
		if err := s.registry.Put(t); err != nil {
			return err
		}
	}
	return nil
}
