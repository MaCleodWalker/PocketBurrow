package burrow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Runtime is the boundary for a future remote node agent.
type Runtime interface {
	Ensure(context.Context, string) (string, error)
}

type Docker struct {
	client *http.Client
	image  string
}

func NewDocker(socket, image string) *Docker {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &Docker{client: &http.Client{Transport: transport, Timeout: 30 * time.Second}, image: image}
}

type containerInfo struct {
	Config struct {
		Image  string
		Labels map[string]string
	}
	State struct {
		Running bool
	}
	HostConfig struct {
		PortBindings map[string][]struct {
			HostIP   string
			HostPort string
		}
	}
	Mounts []struct {
		Type        string
		Name        string
		Destination string
		RW          bool
	}
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string
			HostPort string
		}
	}
}

func (d *Docker) request(ctx context.Context, method, path string, body, out any) (int, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.45"+path, &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		msg, err := io.ReadAll(io.LimitReader(res.Body, 4096))
		if err != nil {
			return res.StatusCode, fmt.Errorf("Docker HTTP %d: %w", res.StatusCode, err)
		}
		return res.StatusCode, fmt.Errorf("Docker HTTP %d: %s", res.StatusCode, msg)
	}
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			return res.StatusCode, err
		}
	}
	return res.StatusCode, nil
}

func (d *Docker) Ensure(ctx context.Context, id string) (string, error) {
	if !tenantID.MatchString(id) {
		return "", errors.New("invalid tenant id")
	}
	name := "pocketburrow-" + id
	path := "/containers/" + url.PathEscape(name)
	var info containerInfo
	status, err := d.request(ctx, http.MethodGet, path+"/json", nil, &info)
	if status == http.StatusNotFound {
		volume := name + "-data"
		var volumeInfo struct {
			Name   string
			Labels map[string]string
		}
		_, err = d.request(ctx, http.MethodPost, "/volumes/create", map[string]any{
			"Name": volume, "Labels": map[string]string{"pocketburrow.tenant": id},
		}, &volumeInfo)
		if err != nil {
			return "", err
		}
		if volumeInfo.Name != volume || volumeInfo.Labels["pocketburrow.tenant"] != id {
			return "", errors.New("existing volume ownership mismatch; refusing to adopt it")
		}
		_, err = d.request(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), map[string]any{
			"Image":        d.image,
			"Labels":       map[string]string{"pocketburrow.tenant": id},
			"ExposedPorts": map[string]any{"8090/tcp": map[string]any{}},
			"HostConfig": map[string]any{
				"RestartPolicy": map[string]string{"Name": "unless-stopped"},
				"PortBindings":  map[string]any{"8090/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": ""}}},
				"Mounts":        []map[string]string{{"Type": "volume", "Source": volume, "Target": "/pb/pb_data"}},
				"CapDrop":       []string{"ALL"},
				"SecurityOpt":   []string{"no-new-privileges:true"},
				"Memory":        int64(512 * 1024 * 1024),
				"NanoCpus":      int64(1_000_000_000),
				"PidsLimit":     int64(128),
				"LogConfig":     map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m", "max-file": "3"}},
			},
		}, nil)
		if err != nil {
			return "", err
		}
		if _, err = d.request(ctx, http.MethodGet, path+"/json", nil, &info); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if info.Config.Labels["pocketburrow.tenant"] != id || info.Config.Image != d.image {
		return "", errors.New("existing container ownership or image mismatch; refusing to adopt it")
	}
	bindings := info.HostConfig.PortBindings["8090/tcp"]
	if len(info.HostConfig.PortBindings) != 1 || len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" {
		return "", errors.New("container configuration must publish only PocketBase on loopback")
	}
	if len(info.Mounts) != 1 || info.Mounts[0].Type != "volume" || info.Mounts[0].Name != name+"-data" ||
		info.Mounts[0].Destination != "/pb/pb_data" || !info.Mounts[0].RW {
		return "", errors.New("container must mount only its own writable tenant data volume")
	}
	if !info.State.Running {
		if _, err := d.request(ctx, http.MethodPost, path+"/start", nil, nil); err != nil {
			return "", err
		}
	}
	if _, err := d.request(ctx, http.MethodGet, path+"/json", nil, &info); err != nil {
		return "", err
	}
	ports := info.NetworkSettings.Ports["8090/tcp"]
	if len(ports) != 1 || ports[0].HostIP != "127.0.0.1" {
		return "", errors.New("container must expose exactly one loopback-only PocketBase port")
	}
	port, err := strconv.Atoi(ports[0].HostPort)
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("invalid Docker port binding")
	}
	return "http://127.0.0.1:" + ports[0].HostPort, nil
}
