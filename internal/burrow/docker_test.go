package burrow

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeDocker(t *testing.T, handler http.Handler) *Docker {
	t.Helper()
	engine := httptest.NewServer(handler)
	t.Cleanup(engine.Close)
	d := NewDocker("/unused", "test/pocketbase:fixed")
	d.client.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(engine.URL, "http://"))
		},
	}
	return d
}

func TestDockerProvisionAndReuse(t *testing.T) {
	created, running := false, false
	createCalls, volumeCalls, startCalls := 0, 0, 0
	d := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1.45/containers/pocketburrow-acme/json":
			if !created {
				http.Error(w, "not found", 404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"Config": map[string]any{"Image": "test/pocketbase:fixed", "Labels": map[string]string{"pocketburrow.tenant": "acme"}},
				"State":  map[string]bool{"Running": running},
				"HostConfig": map[string]any{"PortBindings": map[string]any{
					"8090/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": ""}},
				}},
				"Mounts": []map[string]any{{"Type": "volume", "Name": "pocketburrow-acme-data", "Destination": "/pb/pb_data", "RW": true}},
				"NetworkSettings": map[string]any{"Ports": map[string]any{
					"8090/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "45678"}},
				}},
			})
		case r.Method == "POST" && r.URL.Path == "/v1.45/volumes/create":
			volumeCalls++
			var body struct {
				Name   string
				Labels map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Name != "pocketburrow-acme-data" || body.Labels["pocketburrow.tenant"] != "acme" {
				t.Errorf("bad volume %+v", body)
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(body)
		case r.Method == "POST" && r.URL.Path == "/v1.45/containers/create":
			createCalls++
			var body struct {
				Image      string
				Labels     map[string]string
				HostConfig struct {
					PortBindings map[string][]struct{ HostIP, HostPort string }
					Mounts       []struct{ Type, Source, Target string }
					Memory       int64
					NanoCpus     int64
					PidsLimit    int64
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			ports := body.HostConfig.PortBindings["8090/tcp"]
			if r.URL.Query().Get("name") != "pocketburrow-acme" || body.Image != "test/pocketbase:fixed" ||
				body.Labels["pocketburrow.tenant"] != "acme" || len(ports) != 1 ||
				ports[0].HostIP != "127.0.0.1" || ports[0].HostPort != "" {
				t.Errorf("bad container configuration %+v", body)
			}
			mounts := body.HostConfig.Mounts
			if len(mounts) != 1 || mounts[0].Source != "pocketburrow-acme-data" ||
				mounts[0].Target != "/pb/pb_data" || mounts[0].Type != "volume" ||
				body.HostConfig.Memory != 512*1024*1024 || body.HostConfig.NanoCpus != 1_000_000_000 || body.HostConfig.PidsLimit != 128 {
				t.Errorf("missing isolation limits %+v", body.HostConfig)
			}
			created = true
			w.WriteHeader(201)
		case r.Method == "POST" && r.URL.Path == "/v1.45/containers/pocketburrow-acme/start":
			startCalls++
			running = true
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected Docker call %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	for i := 0; i < 2; i++ {
		endpoint, err := d.Ensure(context.Background(), "acme")
		if err != nil {
			t.Fatal(err)
		}
		if endpoint != "http://127.0.0.1:45678" {
			t.Fatalf("endpoint %q", endpoint)
		}
	}
	if createCalls != 1 || volumeCalls != 1 || startCalls != 1 {
		t.Fatalf("not idempotent: create %d volume %d start %d", createCalls, volumeCalls, startCalls)
	}
}

func TestDockerFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		mutate func(*containerInfo)
	}{
		{name: "engine error", status: 500, body: `{"message":"failed"}`},
		{name: "malformed response", status: 200, body: `{`},
		{name: "wrong owner", status: 200, mutate: func(info *containerInfo) {
			info.Config.Labels["pocketburrow.tenant"] = "beta"
		}},
		{name: "wrong image", status: 200, mutate: func(info *containerInfo) {
			info.Config.Image = "other"
		}},
		{name: "public configured port", status: 200, mutate: func(info *containerInfo) {
			info.State.Running = false
			info.HostConfig.PortBindings["8090/tcp"][0].HostIP = "0.0.0.0"
		}},
		{name: "wrong mounted volume", status: 200, mutate: func(info *containerInfo) {
			info.State.Running = false
			info.Mounts[0].Name = "pocketburrow-beta-data"
		}},
		{name: "public port", status: 200, mutate: func(info *containerInfo) {
			info.NetworkSettings.Ports["8090/tcp"][0].HostIP = "0.0.0.0"
		}},
		{name: "invalid port", status: 200, mutate: func(info *containerInfo) {
			info.NetworkSettings.Ports["8090/tcp"][0].HostPort = "0"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var info containerInfo
			if tc.mutate != nil {
				if err := json.Unmarshal([]byte(`{
					"Config":{"Image":"test/pocketbase:fixed","Labels":{"pocketburrow.tenant":"acme"}},
					"State":{"Running":true},
					"HostConfig":{"PortBindings":{"8090/tcp":[{"HostIp":"127.0.0.1","HostPort":""}]}},
					"Mounts":[{"Type":"volume","Name":"pocketburrow-acme-data","Destination":"/pb/pb_data","RW":true}],
					"NetworkSettings":{"Ports":{"8090/tcp":[{"HostIp":"127.0.0.1","HostPort":"45678"}]}}
				}`), &info); err != nil {
					t.Fatal(err)
				}
				tc.mutate(&info)
			}
			d := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("mutated Docker on invalid existing container")
				}
				w.WriteHeader(tc.status)
				if tc.mutate != nil {
					json.NewEncoder(w).Encode(info)
				} else {
					w.Write([]byte(tc.body))
				}
			}))
			if _, err := d.Ensure(context.Background(), "acme"); err == nil {
				t.Fatal("unsafe container or Docker error accepted")
			}
		})
	}
	d := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("invalid id reached Docker")
	}))
	if _, err := d.Ensure(context.Background(), "../escape"); err == nil {
		t.Fatal("invalid tenant id accepted")
	}
}

func TestDockerRejectsVolumeCollision(t *testing.T) {
	d := fakeDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.45/containers/pocketburrow-acme/json":
			http.Error(w, "not found", 404)
		case "/v1.45/volumes/create":
			w.WriteHeader(201)
			io.WriteString(w, `{"Name":"pocketburrow-acme-data","Labels":{"pocketburrow.tenant":"beta"}}`)
		default:
			t.Errorf("created a container with another tenant's volume")
			http.Error(w, "unexpected", 500)
		}
	}))
	if _, err := d.Ensure(context.Background(), "acme"); err == nil || !strings.Contains(err.Error(), "volume ownership") {
		t.Fatalf("volume collision: %v", err)
	}
}
