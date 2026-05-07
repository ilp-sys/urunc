package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	containersAPI "github.com/containerd/containerd/api/services/containers/v1"
	contentAPI "github.com/containerd/containerd/api/services/content/v1"
	imagesAPI "github.com/containerd/containerd/api/services/images/v1"
	types "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/images"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	imageSpec "github.com/opencontainers/image-spec/specs-go/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func getConnection(ctx context.Context) (*grpc.ClientConn, error) {
	addr := os.Getenv("CONTAINERD_ADDRESS")
	if addr == "" {
		addr = os.Getenv("ADDRESS")
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

func fetchUruncAnnotations(
	ctx context.Context,
	conn *grpc.ClientConn,
	containerID string,
) (map[string]string, error) {
	imageEntrypoint, err := getImageEntrypoint(ctx, conn, containerID)
	if err != nil {
		return nil, err
	}

	uruncAnnotations, err := fetchUruncAnnotationsFromImageEntryPoint(ctx, conn, imageEntrypoint)
	if err != nil {
		return nil, fmt.Errorf("fetch from containerd content store: %w", err)
	}

	return uruncAnnotations, nil
}

func getImageEntrypoint(
	ctx context.Context,
	conn *grpc.ClientConn,
	containerID string,
) (*types.Descriptor, error) {
	containersClient := containersAPI.NewContainersClient(conn)

	containersResp, err := containersClient.Get(ctx, &containersAPI.GetContainerRequest{
		ID: containerID,
	})
	if err != nil {
		return nil, fmt.Errorf("container %s has no image ref in containerd content store: %w", containerID, err)
	}

	imageRef := containersResp.Container.Image
	if imageRef == "" {
		log.G(ctx).Warn("urunc(shim): failed to get image reference from containerd; trying Docker image store")

		imageRef, err = fetchImageReferenceFromDockerImageStore(ctx, containerID)
		if err != nil {
			return nil, fmt.Errorf("failed to get image reference for container %s from Docker image store: %w", containerID, err)
		}
	}

	imageClient := imagesAPI.NewImagesClient(conn)

	imageResp, err := imageClient.Get(ctx, &imagesAPI.GetImageRequest{
		Name: imageRef,
	})
	if err != nil {
		return nil, fmt.Errorf("get image %s from containerd image service: %w", imageRef, err)
	}

	return imageResp.Image.Target, nil
}

func fetchUruncAnnotationsFromImageEntryPoint(
	ctx context.Context,
	conn *grpc.ClientConn,
	imageEntrypoint *types.Descriptor,
) (map[string]string, error) {
	contentClient := contentAPI.NewContentClient(conn)

	manifestDesc, err := resolveManifestDescriptor(ctx, contentClient, imageEntrypoint)
	if err != nil {
		return nil, err
	}

	manifestRaw, err := readBlob(ctx, contentClient, manifestDesc.Digest, manifestDesc.Size)
	if err != nil {
		return nil, fmt.Errorf("read manifest blob: %w", err)
	}

	var manifest imageSpec.Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}

	configRaw, err := readBlob(ctx, contentClient, manifest.Config.Digest.String(), manifest.Config.Size)
	if err != nil {
		return nil, fmt.Errorf("read config blob: %w", err)
	}

	var imageCfg imageSpec.Image
	if err := json.Unmarshal(configRaw, &imageCfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// Config labels are used first.
	filtered := filterUruncLabels(imageCfg.Config.Labels)

	// Manifest annotations have higher precedence over config labels.
	for k, v := range filterUruncLabels(manifest.Annotations) {
		filtered[k] = v
	}

	return filtered, nil
}

func resolveManifestDescriptor(
	ctx context.Context,
	contentClient contentAPI.ContentClient,
	desc *types.Descriptor,
) (*types.Descriptor, error) {
	if images.IsManifestType(desc.MediaType) {
		return desc, nil
	}

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

		for _, manifest := range index.Manifests {
			if manifest.Platform == nil {
				continue
			}

			if matcher.Match(*manifest.Platform) {
				return &types.Descriptor{
					MediaType: manifest.MediaType,
					Digest:    manifest.Digest.String(),
					Size:      manifest.Size,
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
