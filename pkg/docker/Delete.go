package docker

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	commonIL "github.com/intertwin-eu/interlink-docker-plugin/pkg/common"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"

	"path/filepath"
)

// DeleteHandler stops and deletes Docker containers from provided data
func (h *SidecarHandler) DeleteHandler(w http.ResponseWriter, r *http.Request) {
	log.G(h.Ctx).Info("\u23F3 [DELETE CALL] Received delete call from Interlink")

	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	_, span := tracer.Start(h.Ctx, "Delete", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))

	var execReturn exec.ExecResult
	statusCode := http.StatusOK
	bodyBytes, err := io.ReadAll(r.Body)

	if err != nil {
		statusCode = http.StatusInternalServerError
		log.G(h.Ctx).Error(err)
		w.WriteHeader(statusCode)
		w.Write([]byte("Some errors occurred while deleting container. Check Docker Sidecar's logs"))
		return
	}

	var pod v1.Pod
	err = json.Unmarshal(bodyBytes, &pod)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		w.Write([]byte("Some errors occurred while creating container. Check Docker Sidecar's logs"))
		log.G(h.Ctx).Error(err)
		return
	}

	podUID := string(pod.UID)
	podNamespace := string(pod.Namespace)

	for _, container := range pod.Spec.Containers {
		containerName := podNamespace + "-" + podUID + "-" + container.Name
		// if the FPGA manager is nil we don't need to release the container
		if h.FPGAManager != nil {
			// release the container from the FPGA manager
			err = h.FPGAManager.Release(containerName)
			if err != nil {
				log.G(h.Ctx).Error("\u274C [DELETE CALL] Error releasing container " + containerName)
			}
		}
		// same for the GPU manager: it is nil on nodes started without GPU support
		if h.GpuManager != nil {
			err = h.GpuManager.Release(containerName)
			if err != nil {
				log.G(h.Ctx).Error("\u274C [DELETE CALL] Error releasing GPUs of container " + containerName)
			}
		}
	}

	log.G(h.Ctx).Debug("\u2705 [DELETE CALL] Deleting POD " + podUID + "_dind")

	cmd := []string{"rm", "-f", podUID + "_dind"}
	shell := exec.ExecTask{
		Command: "docker",
		Args:    cmd,
		Shell:   true,
	}
	execReturn, _ = shell.Execute()
	execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

	// go-execute reports a nil error for non-zero exits, so inspect ExitCode
	// rather than Stderr (docker prints benign warnings to stderr on success).
	// Removing a missing container is NOT treated as fatal: a pod can fail before
	// its DIND is ever created, and delete must stay idempotent \u2014 returning 500
	// here would make interLink retry the delete forever.
	if execReturn.ExitCode != 0 {
		log.G(h.Ctx).Warning("\u26A0\uFE0F  [DELETE CALL] docker rm -f " + podUID + "_dind exited " +
			strconv.Itoa(execReturn.ExitCode) + ": " + strings.TrimSpace(execReturn.Stderr))
	} else {
		log.G(h.Ctx).Info("\u2705 [DELETE CALL] Deleted container " + podUID + "_dind")
	}

	dindSpec, lookupErr := h.DindManager.GetDindFromPodUID(podUID)
	if lookupErr != nil {
		// The in-memory entry may already be gone (a failed create cleaned it, or
		// it never got a pod UID). We cannot name its network here, but the
		// orphan-network reaper below reclaims any unattached _dind_network.
		log.G(h.Ctx).Info("\u2139\uFE0F  [DELETE CALL] No DIND entry for pod " + podUID + " (already removed?)")
	} else {
		log.G(h.Ctx).Info("\u2705 [DELETE CALL] Retrieved DindSpecs: " + dindSpec.DindID + " " + dindSpec.PodUID + " " + dindSpec.DindNetworkID)

		// RemoveDindFromList removes the bridge network and returns the subnet to
		// the pool. The DIND container was already force-removed above, so its
		// network no longer has an attached endpoint and can now be deleted.
		if remErr := h.DindManager.RemoveDindFromList(dindSpec.PodUID); remErr != nil {
			log.G(h.Ctx).Error("\u274C [DELETE CALL] Error removing DIND from list: " + remErr.Error())
		}
	}

	// Best-effort: reclaim any orphan DIND networks left behind by a create that
	// failed before its entry was fully registered.
	h.DindManager.ReapOrphanNetworks()

	podDirectoryPathToDelete := filepath.Join(h.Config.DataRootFolder, podNamespace+"-"+podUID)
	log.G(h.Ctx).Info("\u2705 [DELETE CALL] Deleting directory " + podDirectoryPathToDelete)

	err = os.RemoveAll(podDirectoryPathToDelete)

	w.WriteHeader(statusCode)
	if statusCode != http.StatusOK {
		w.Write([]byte("Some errors occurred deleting containers. Check Docker Sidecar's logs"))
	} else {
		w.Write([]byte("All containers for submitted Pods have been deleted"))
	}

	if err != nil {
		span.SetAttributes(attribute.String("error", err.Error()))
	}
	commonIL.SetDurationSpan(start, span, commonIL.WithHTTPReturnCode(statusCode))
	span.End()
}
