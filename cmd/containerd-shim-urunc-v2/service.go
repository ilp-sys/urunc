package main

import (
	"context"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	m "github.com/urunc-dev/urunc/internal/metrics"
)

type uruncTaskService struct {
	taskAPI.TaskService
}

func (s *uruncTaskService) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTaskService(server, s)
	return nil
}

func (s *uruncTaskService) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	metrics := m.NewZerologMetrics(true, "/tmp/urunc-shim-metrics.log", r.ID)
	if metrics == nil {
		metrics = m.NewMockMetrics(r.ID)
	}

	metrics.SetLoggerContainerID(r.ID)
	metrics.Capture(m.TS19) // SH.create_invoked

	conn, err := getConnection(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Warn("urunc(shim): failed to get containerd connection")
		return nil, err
	}
	defer conn.Close()

	log.G(ctx).Info("urunc(shim): successfully connected to containerd")

	ns, ok := namespaces.Namespace(ctx)
	if !ok {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)

	metrics.Capture(m.TS20) // SH.containerd_metadata_fetch_start

	uruncAnnotations, err := fetchUruncAnnotations(ctx, conn, r.ID)

	metrics.Capture(m.TS21) // SH.containerd_metadata_fetch_done

	if err != nil {
		log.G(ctx).WithError(err).Warn("urunc(shim): failed to fetch annotations from containerd")
	}

	if len(uruncAnnotations) > 0 {
		if err := patchConfigJSON(ctx, r.Bundle, uruncAnnotations); err != nil {
			log.G(ctx).WithError(err).Warn("urunc(shim): failed to patch config.json")
		}
	}

	metrics.Capture(m.TS22) // SH.config_patch_done

	return s.TaskService.Create(ctx, r)
}
