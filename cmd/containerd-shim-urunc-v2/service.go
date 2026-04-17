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
	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	contentapi "github.com/containerd/containerd/api/services/content/v1"
	imagesapi "github.com/containerd/containerd/api/services/images/v1"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	imgspec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"

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
		ns, _ := namespaces.Namespace(ctx)
		annotations, err := fetchManifestAnnotations(ctx, ttrpcAddr, r.ID, ns)
		if err != nil {
			log.G(ctx).WithError(err).Warn("failed to fetch manifest annotations")
		} else if len(annotations) > 0 {
			if err := patchConfigJSON(ctx, r.Bundle, annotations); err != nil {
				log.G(ctx).WithError(err).Warn("failed to patch Config.json")
			}
		}
	}

	resp, _ := s.TaskService.Create(ctx, r)
	return resp, nil
}

func fetchManifestAnnotations(ctx context.Context, ttrpcAddr, containerID, ns string) (map[string]string, error) {
	grpcAddr := grpcAddressFromTTRPC(ttrpcAddr)

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(
		dialCtx,
		"unix://"+grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, target string) (net.Conn, error) {
			// target is like unix:///run/containerd/containerd.sock
			path := strings.TrimPrefix(target, "unix://")
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", grpcAddr, err)
	}
	defer conn.Close()

	ctx = namespaces.WithNamespace(ctx, ns)

	cClient := containersapi.NewContainersClient(conn)
	cResp, err := cClient.Get(ctx, &containersapi.GetContainerRequest{ID: containerID})
	if err != nil {
		return nil, fmt.Errorf("get container: %w", err)
	}

	imageRef := cResp.Container.Image
	if imageRef == "" {
		return nil, fmt.Errorf("no image ref on container %s", containerID)
	}

	iClient := imagesapi.NewImagesClient(conn)
	iResp, err := iClient.Get(ctx, &imagesapi.GetImageRequest{Name: imageRef})
	if err != nil {
		return nil, fmt.Errorf("get image: %w", err)
	}

	target := iResp.Image.Target
	log.G(ctx).Infof(
		"urunc: image target digest=%s size=%d mediaType=%s",
		target.Digest, target.Size, target.MediaType,
	)

	csClient := contentapi.NewContentClient(conn)
	stream, err := csClient.Read(ctx, &contentapi.ReadContentRequest{
		Digest: target.Digest,
		Size:   target.Size,
	})
	if err != nil {
		return nil, fmt.Errorf("content read: %w", err)
	}

	var raw []byte
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("stream recv: %w", err)
		}
		raw = append(raw, resp.Data...)
	}

	var manifest imgspec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}

	result := make(map[string]string)
	for k, v := range manifest.Annotations {
		if strings.HasPrefix(k, uruncPrefix) {
			result[k] = v
		}
	}

	return result, nil
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

func patchConfigJSON(ctx context.Context, bundlePath string, annotations map[string]string) error {
	configPath := filepath.Join(bundlePath, "config.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config.json: %w", err)
	}

	var spec runtimespec.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("unmarshal spec: %w", err)
	}

	for k := range spec.Annotations {
		if strings.HasPrefix(k, uruncPrefix) {
			log.G(ctx).Infof("urunc: config already contains %s, skipping patch", k)
			return nil
		}
	}

	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}

	for k, v := range annotations {
		spec.Annotations[k] = v
		log.G(ctx).Infof("urunc: patch add %s=%q", k, v)
	}
	log.G(ctx).Infof("urunc: annotations count after merge=%d", len(spec.Annotations))

	patched, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal spec: %w", err)
	}
	log.G(ctx).Infof("urunc: patched config.json bytes=%d", len(patched))

	if err := os.WriteFile(configPath, patched, 0600); err != nil {
		return fmt.Errorf("write config.json: %w", err)
	}
	return nil
}
