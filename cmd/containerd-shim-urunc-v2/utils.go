package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	contentAPI "github.com/containerd/containerd/api/services/content/v1"
	"github.com/containerd/log"
	runtimeSpec "github.com/opencontainers/runtime-spec/specs-go"
)

const uruncPrefix = "com.urunc.unikernel."

func readBlob(
	ctx context.Context,
	contentClient contentAPI.ContentClient,
	digest string,
	size int64,
) ([]byte, error) {
	stream, err := contentClient.Read(ctx, &contentAPI.ReadContentRequest{
		Digest: digest,
		Size:   size,
	})
	if err != nil {
		return nil, err
	}

	var raw []byte
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		raw = append(raw, resp.Data...)
	}

	return raw, nil
}

func patchConfigJSON(
	ctx context.Context,
	bundlePath string,
	annotations map[string]string,
) error {
	configPath := filepath.Join(bundlePath, "config.json")

	fi, err := os.Stat(configPath)
	if err != nil {
		return fmt.Errorf("stat config.json: %w", err)
	}
	mode := fi.Mode()

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config.json: %w", err)
	}

	var spec runtimeSpec.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("unmarshal spec: %w", err)
	}

	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}

	for k, v := range annotations {
		if _, exists := spec.Annotations[k]; exists {
			continue
		}

		spec.Annotations[k] = v
	}

	patched, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal spec: %w", err)
	}

	log.G(ctx).Infof("urunc(shim): patched config.json bytes=%d", len(patched))

	if err := os.WriteFile(configPath, patched, mode); err != nil {
		return fmt.Errorf("write config.json: %w", err)
	}

	return nil
}

func filterUruncLabels(labels map[string]string) map[string]string {
	filtered := make(map[string]string)

	for k, v := range labels {
		if strings.HasPrefix(k, uruncPrefix) {
			filtered[k] = v
		}
	}

	return filtered
}
