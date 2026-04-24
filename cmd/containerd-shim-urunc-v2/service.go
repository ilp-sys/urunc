package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	containersAPI "github.com/containerd/containerd/api/services/containers/v1"
	contentAPI "github.com/containerd/containerd/api/services/content/v1"
	imagesAPI "github.com/containerd/containerd/api/services/images/v1"
	types "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/containerd/platforms"
	"github.com/containerd/containerd/images"
	imageSpec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimeSpec "github.com/opencontainers/runtime-spec/specs-go"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const uruncPrefix = "com.urunc.unikernel."

type uruncTaskService struct {
	taskAPI.TaskService
}

func (s *uruncTaskService) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTaskService(server, s)
	return nil
}

func (s *uruncTaskService) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	conn, err := getConnection(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Warn("urunc(shim): failed to get contaienrd connection")
		return nil, err
	}
	defer conn.Close()
	log.G(ctx).Info("urunc(shim): successfully connected to containerd")

	ns, ok := namespaces.Namespace(ctx)
	if !ok {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)

	imageEntrypoint, err := getImageEntrypoint(ctx, conn, r.ID)
	if err != nil {
		return nil, err
	}

	uruncAnnotations, err := fetchUruncAnnotations(ctx, conn, imageEntrypoint)
	if err != nil {
		return nil, fmt.Errorf("fetch urunc annotations: %w", err)
	}
	if len(uruncAnnotations) > 0 {
		if err := patchConfigJSON(ctx, r.Bundle, uruncAnnotations); err != nil {
			log.G(ctx).WithError(err).Warn("failed to patch Config.json")
		}
	}

	return s.TaskService.Create(ctx, r)
}

func getConnection(ctx context.Context) (*grpc.ClientConn, error) {
	addr := os.Getenv("CONTAINERD_ADDRESS")
	if addr == "" {
		addr =  os.Getenv("ADDRESS")
	}
	if addr == "" {
		addr = "/run/containerd/containerd.sock"
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(
		dialCtx,
		"unix://"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, target string) (net.Conn, error) {
			path := strings.TrimPrefix(target, "unix://")
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}

	return conn, nil
}

func getImageEntrypoint(ctx context.Context, conn *grpc.ClientConn, containerID string) (*types.Descriptor, error) {
	containersClient := containersAPI.NewContainersClient(conn)
	containersResp, err := containersClient.Get(ctx, &containersAPI.GetContainerRequest{ID: containerID})
	if err != nil {
		return nil, fmt.Errorf("container %s has no image ref: %w", containerID, err)
	}

	imageRef := containersResp.Container.Image
	if imageRef == "" {
		return nil, fmt.Errorf("container %s has empty image ref", containerID)
	}

	imageClient := imagesAPI.NewImagesClient(conn)
	imageResp, err := imageClient.Get(ctx, &imagesAPI.GetImageRequest{Name: imageRef})
	if err != nil {
		return nil, err
	}

	return imageResp.Image.Target, nil
}

func fetchUruncAnnotations(
	ctx context.Context,
	conn *grpc.ClientConn,
	imageEntrypoint *types.Descriptor,
) (map[string]string, error) {
	contentClient := contentAPI.NewContentClient(conn)

	manifestDesc, err := resolveManifestDescriptor(ctx, contentClient, imageEntrypoint)
	if err != nil {
		return nil, err
	}

	// search Manifest Annotations
	manifestRaw, err := readBlob(ctx, contentClient, manifestDesc.Digest, imageEntrypoint.Size)
	if err != nil {
		return nil, fmt.Errorf("read manifest blob: %w", err)
	}
	var manifest imageSpec.Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}

	// search Config Labels
	configRaw, err := readBlob(ctx, contentClient, manifest.Config.Digest.String(), manifest.Config.Size)
	if err != nil {
		return nil, fmt.Errorf("read config blob: %w", err)
	}
	var imageCfg imageSpec.Image
	if err := json.Unmarshal(configRaw, &imageCfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// filter urunc annotations only
	// manifest annotations has higher precedence over config labels
	filtered := make(map[string]string)
	for k, v := range imageCfg.Config.Labels {
		if strings.HasPrefix(k, uruncPrefix) {
			filtered[k] = v
		}
	}
	for k, v := range manifest.Annotations {
		if strings.HasPrefix(k, uruncPrefix) {
			filtered[k] = v
		}
	}

	return filtered, nil
}

func resolveManifestDescriptor(
	ctx context.Context,
	contentClient contentAPI.ContentClient,
	desc *types.Descriptor,
) (*types.Descriptor, error) {
	// single-arch image: target already points to a manifest
	if images.IsManifestType(desc.MediaType) {
		return desc, nil
	}

	// multi-arch image: target points to an index / manifest list
	if images.IsIndexType(desc.MediaType) {
		indexRaw, err := readBlob(ctx, contentClient, desc.Digest, desc.Size)
		if err != nil {
			return nil, fmt.Errorf("read image index blob: %w", err)
		}

		var index imageSpec.Index
		if err := json.Unmarshal(indexRaw, &index); err != nil {
			return nil, fmt.Errorf("unmarshal image index: %w", err)
		}

		matcher := platforms.DefaultStrict()

		for _, m := range index.Manifests {
			if m.Platform == nil {
				continue
			}

			if matcher.Match(*m.Platform) {
				return &types.Descriptor{
					MediaType: m.MediaType,
					Digest:    m.Digest.String(),
					Size:      m.Size,
				}, nil
			}
		}

		return nil, fmt.Errorf(
			"no matching manifest found in image index for platform %s",
			platforms.Format(platforms.DefaultSpec()),
		)
	}

	return nil, fmt.Errorf("unsupported image target media type: %s", desc.MediaType)
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
