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
	ttrpcAddr := os.Getenv("TTRPC_ADDRESS")

	if ttrpcAddr == "" {
		log.G(ctx).Warn("TTRPC_ADDRESS not set, skipping annot patch")
	} else {
		conn, err := getGRPCConnection(ctx, ttrpcAddr)
		if err != nil {
			log.G(ctx).WithError(err).Warn("failed to fetch manifest annotations")
			return nil, err
		}
		defer conn.Close()

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
	}

	return s.TaskService.Create(ctx, r)
}

func getGRPCConnection(ctx context.Context, ttrpcAddr string) (*grpc.ClientConn, error) {
	grpcAddr := grpcAddressFromTTRPC(ttrpcAddr)

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(
		dialCtx,
		"unix://"+grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, target string) (net.Conn, error) {
			path := strings.TrimPrefix(target, "unix://")
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", grpcAddr, err)
	}

	return conn, nil
}

func grpcAddressFromTTRPC(ttrpcAddr string) string {
	// usually:
	//   /run/containerd/containerd.sock.ttrpc -> /run/containerd/containerd.sock
	if strings.HasSuffix(ttrpcAddr, ".ttrpc") {
		return strings.TrimSuffix(ttrpcAddr, ".ttrpc")
	}
	// fallback
	return "/run/containerd/containerd.sock"
}

func getImageEntrypoint(ctx context.Context, conn *grpc.ClientConn, containerID string) (*types.Descriptor, error) {
	containersClient := containersAPI.NewContainersClient(conn)
	containersResp, err := containersClient.Get(ctx, &containersAPI.GetContainerRequest{ID: containerID})
	if err != nil {
		return nil, fmt.Errorf("container %s has no image ref: %w", containerID, err)
	}

	imageRef := containersResp.Container.Image
	if imageRef == "" {
		return nil, err
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

	// search Manifest Annotations
	// TODO: check if imageEntrypoint.MediaType is manifest or index, and handle index case
	manifestRaw, err := readBlob(ctx, contentClient, imageEntrypoint.Digest, imageEntrypoint.Size)
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
	filtered := make(map[string]string)
	for k, v := range manifest.Annotations {
		if strings.HasPrefix(k, uruncPrefix) {
			filtered[k] = v
		}
	}
	for k, v := range imageCfg.Config.Labels {
		if strings.HasPrefix(k, uruncPrefix) {
			filtered[k] = v
		}
	}

	return filtered, nil
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
	log.G(ctx).Infof("urunc: patched config.json bytes=%d", len(patched))

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
