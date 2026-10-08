package burrow

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func TestRegistryPersistsAndLocks(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenRegistry(dir); err == nil {
		other.Close()
		t.Fatal("second registry acquired lock")
	}
	want := Tenant{ID: "acme", Host: "acme.localhost", State: "provisioning"}
	if err := r.Put(want); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, ok := r.Get("acme")
	if !ok || got != want {
		t.Fatalf("persisted tenant = %+v, exists %v", got, ok)
	}
	info, err := os.Stat(filepath.Join(dir, "tenants.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("registry permissions = %o", info.Mode().Perm())
	}
}

func TestRegistryRejectsCorruption(t *testing.T) {
	for _, data := range []string{`{`, `null`, `{"other":{"id":"acme","host":"acme.localhost","state":"ready"}}`,
		`{"acme":{"id":"acme","host":"acme.localhost","state":"unexpected"}}`} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "tenants.json"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if r, err := OpenRegistry(dir); err == nil {
				r.Close()
				t.Fatal("corrupt registry accepted")
			}
		})
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := testRegistry(t)
	var wg sync.WaitGroup
	for _, id := range []string{"acme", "beta", "gamma"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Put(Tenant{ID: id, Host: id + ".localhost", State: "provisioning"}); err != nil {
				t.Error(err)
			}
			r.Get(id)
			r.List()
		}()
	}
	wg.Wait()
	if len(r.List()) != 3 {
		t.Fatal("lost concurrent updates")
	}
}
