package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/containerd/log"
)

func fetchImageReferenceFromDockerImageStore(
	ctx context.Context,
	containerID string,
) (string, error) {
	log.G(ctx).Infof("urunc(shim): Docker fallback entered: containerID=%q", containerID)

	client, err := newDockerClient(ctx)
	if err != nil {
		return "", fmt.Errorf("create Docker client: %w", err)
	}

	log.G(ctx).Infof("urunc(shim): Docker connection made: containerID=%q", containerID)

	containerPath := "/containers/" + url.PathEscape(containerID) + "/json"

	log.G(ctx).Infof(
		"urunc(shim): trying Docker container inspect: containerID=%q path=%q",
		containerID,
		containerPath,
	)

	container, err := dockerGet[dockerContainerInspect](ctx, client, containerPath)
	if err != nil {
		return "", fmt.Errorf(
			"Docker container inspect failed; this likely means Docker daemon re-entry during create path: containerID=%s: %w",
			containerID,
			err,
		)
	}

	log.G(ctx).Infof(
		"urunc(shim): Docker container inspect succeeded: containerID=%q Config.Image=%q Image=%q containerLabels=%d",
		containerID,
		container.Config.Image,
		container.Image,
		len(container.Config.Labels),
	)

	return container.Config.Image, nil
}

type dockerClient struct {
	httpClient *http.Client
	host       string
}

type dockerContainerInspect struct {
	Image  string `json:"Image"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

type dockerImageInspect struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func newDockerClient(ctx context.Context) (*dockerClient, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	if !strings.HasPrefix(host, "unix://") {
		return nil, fmt.Errorf("unsupported DOCKER_HOST %q: only unix sockets are supported", host)
	}

	socketPath := strings.TrimPrefix(host, "unix://")
	if socketPath == "" {
		return nil, fmt.Errorf("empty Docker socket path")
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	return &dockerClient{
		httpClient: client,
		host:       host,
	}, nil
}

func dockerGet[T any](ctx context.Context, client *dockerClient, path string) (*T, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Docker API GET %s via %s: %w", path, client.host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf(
			"Docker API GET %s failed: %s: %s",
			path,
			resp.Status,
			strings.TrimSpace(string(body)),
		)
	}

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode Docker API response %s: %w", path, err)
	}

	return &out, nil
}
