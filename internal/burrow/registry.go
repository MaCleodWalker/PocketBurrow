package burrow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

var tenantID = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type Tenant struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	State     string    `json:"state"`
	Endpoint  string    `json:"endpoint,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Registry struct {
	mu      sync.RWMutex
	path    string
	lock    *os.File
	tenants map[string]Tenant
}

func OpenRegistry(dir string) (*Registry, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "registry.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("registry already in use: %w", err)
	}
	r := &Registry{path: filepath.Join(dir, "tenants.json"), lock: lock, tenants: make(map[string]Tenant)}
	data, err := os.ReadFile(r.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		r.Close()
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &r.tenants); err != nil {
			r.Close()
			return nil, fmt.Errorf("invalid registry: %w", err)
		}
		if r.tenants == nil {
			r.Close()
			return nil, errors.New("invalid null registry")
		}
		for id, t := range r.tenants {
			if id != t.ID || !tenantID.MatchString(id) || t.Host == "" ||
				(t.State != "provisioning" && t.State != "ready" && t.State != "error") {
				r.Close()
				return nil, fmt.Errorf("invalid registry tenant %q", id)
			}
		}
	}
	return r, nil
}

func (r *Registry) Close() error { return r.lock.Close() }

func (r *Registry) Get(id string) (Tenant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tenants[id]
	return t, ok
}

func (r *Registry) List() []Tenant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tenant, 0, len(r.tenants))
	for _, t := range r.tenants {
		out = append(out, t)
	}
	return out
}

func (r *Registry) Put(t Tenant) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]Tenant, len(r.tenants)+1)
	for id, tenant := range r.tenants {
		next[id] = tenant
	}
	next[t.ID] = t
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".tenants-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		return err
	}
	r.tenants = next
	dir, err := os.Open(filepath.Dir(r.path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
