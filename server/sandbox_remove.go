package server

import (
	"fmt"

	"github.com/containers/storage"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	oci "github.com/cri-o/cri-o/internal/oci"
	pkgstorage "github.com/cri-o/cri-o/internal/storage"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// RemovePodSandbox deletes the sandbox. If there are any running containers in the
// sandbox, they should be force deleted.
func (s *Server) RemovePodSandbox(ctx context.Context, req *types.RemovePodSandboxRequest) error {
	log.Infof(ctx, "Removing pod sandbox: %s", req.PodSandboxID)
	sb, err := s.getPodSandboxFromRequest(req.PodSandboxID)
	if err != nil {
		if err == sandbox.ErrIDEmpty {
			return err
		}
		if err == errSandboxNotCreated {
			return fmt.Errorf("sandbox %s is not yet created", req.PodSandboxID)
		}

		// If the sandbox isn't found we just return an empty response to adhere
		// the CRI interface which expects to not error out in not found
		// cases.
		log.Warnf(ctx, "could not get sandbox %s, it's probably been removed already: %v", req.PodSandboxID, err)
		return nil
	}

	containers := sb.Containers().List()

	// Delete all the containers in the sandbox
	for _, c := range containers {
		if err := s.removeContainerInPod(ctx, sb, c); err != nil {
			return err
		}
	}

	s.removeInfraContainer(sb.InfraContainer())
	if err := s.removeContainerInPod(ctx, sb, sb.InfraContainer()); err != nil {
		return err
	}

	// Cleanup network resources for this pod
	if err := s.networkStop(ctx, sb); err != nil {
		return errors.Wrap(err, "stop pod network")
	}

	if err := sb.UnmountShm(); err != nil {
		return errors.Wrap(err, "unable to unmount SHM")
	}

	if err := s.StorageRuntimeServer().RemovePodSandbox(sb.ID()); err != nil && err != pkgstorage.ErrInvalidSandboxID {
		return fmt.Errorf("failed to remove pod sandbox %s: %v", sb.ID(), err)
	}
	if err := sb.RemoveManagedNamespaces(); err != nil {
		return errors.Wrap(err, "unable to remove managed namespaces")
	}

	s.ReleasePodName(sb.Name())
	if err := s.removeSandbox(sb.ID()); err != nil {
		log.Warnf(ctx, "failed to remove sandbox: %v", err)
	}
	if err := s.PodIDIndex().Delete(sb.ID()); err != nil {
		return fmt.Errorf("failed to delete pod sandbox %s from index: %v", sb.ID(), err)
	}

	log.Infof(ctx, "Removed pod sandbox: %s", sb.ID())
	return nil
}

func (s *Server) removeContainerInPod(ctx context.Context, sb *sandbox.Sandbox, c *oci.Container) error {
	if !sb.Stopped() {
		cState := c.State()
		if cState.Status == oci.ContainerStateCreated || cState.Status == oci.ContainerStateRunning {
			timeout := int64(10)
			if err := s.Runtime().StopContainer(ctx, c, timeout); err != nil {
				// Assume container is already stopped
				log.Warnf(ctx, "failed to stop container %s: %v", c.Name(), err)
			}
			if err := s.Runtime().WaitContainerStateStopped(ctx, c); err != nil {
				return fmt.Errorf("failed to get container 'stopped' status %s in pod sandbox %s: %v", c.Name(), sb.ID(), err)
			}
		}
	}

	if err := s.Runtime().DeleteContainer(ctx, c); err != nil {
		return fmt.Errorf("failed to delete container %s in pod sandbox %s: %v", c.Name(), sb.ID(), err)
	}

	c.CleanupConmonCgroup()

	if !c.Spoofed() {
		if err := s.StorageRuntimeServer().StopContainer(c.ID()); err != nil && err != storage.ErrContainerUnknown {
			// assume container already umounted
			log.Warnf(ctx, "failed to stop container %s in pod sandbox %s: %v", c.Name(), sb.ID(), err)
		}
		if err := s.StorageRuntimeServer().DeleteContainer(c.ID()); err != nil && err != storage.ErrContainerUnknown {
			return fmt.Errorf("failed to delete container %s in pod sandbox %s: %v", c.Name(), sb.ID(), err)
		}
	}

	s.ReleaseContainerName(c.Name())
	s.removeContainer(c)
	if err := s.CtrIDIndex().Delete(c.ID()); err != nil {
		return fmt.Errorf("failed to delete container %s in pod sandbox %s from index: %v", c.Name(), sb.ID(), err)
	}
	sb.RemoveContainer(c)

	return nil
}
