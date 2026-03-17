package docker

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"

	commonIL "github.com/intertwin-eu/interlink-docker-plugin/pkg/common"
)

// StatusHandler checks Docker Container's status by running docker ps -af command and returns that status
func (h *SidecarHandler) StatusHandler(w http.ResponseWriter, r *http.Request) {
	log.G(h.Ctx).Info("\u23F3 [STATUS CALL] received get status call")

	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	_, span := tracer.Start(h.Ctx, "Status", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))

	var resp []commonIL.PodStatus
	var req []*v1.Pod
	statusCode := http.StatusOK

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
		log.G(h.Ctx).Error(err)
		return
	}

	err = json.Unmarshal(bodyBytes, &req)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
		log.G(h.Ctx).Error(err)
		return
	}

	for i, pod := range req {

		podUID := string(pod.UID)
		podNamespace := string(pod.Namespace)

		log.G(h.Ctx).Info("\u2705 [STATUS CALL] Pod UID: ", podUID)

		// send a docker command to retrieve the uuid of the dind container
		// the command to exec is: docker inspect --format '{{.Id}}' podUID + "_dind"
		cmd := []string{"inspect", "--format", "{{.Id}}", podUID + "_dind"}
		shell := exec.ExecTask{
			Command: "docker",
			Args:    cmd,
			Shell:   true,
		}
		execReturn, err := shell.Execute()
		if err != nil {
			log.G(h.Ctx).Error(err)
			statusCode = http.StatusInternalServerError
			break
		}

		//dindUUID := strings.ReplaceAll(execReturn.Stdout, "\n", "")
		dindUUID := strings.Join(strings.Fields(execReturn.Stdout), "")
		log.G(h.Ctx).Info("\u2705 [STATUS CALL] UUID of the dind container retrieved successfully: ", dindUUID)

		// if the string is empty or the length of the string is 0, return an error and 404 status code\
		if len(dindUUID) == 0 || dindUUID == "" {
			log.G(h.Ctx).Error("\u274C [STATUS CALL] Error retrieving UUID of the dind container")
			statusCode = http.StatusNotFound
			w.WriteHeader(statusCode)
			w.Write([]byte("DIND container with UUID " + dindUUID + " not found. Maybe it was deleted or never existed."))
			return
		}

		resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: podUID, PodNamespace: podNamespace, JobID: dindUUID})

		disabledInitContainers := make(map[string]bool)
		if ann, ok := pod.Annotations["interlink.eu/disable-offload-init-containers"]; ok {
			for _, name := range strings.Split(ann, ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					disabledInitContainers[name] = true
				}
			}
		}

		// check if the pod has initContainers and get their status
		for _, container := range pod.Spec.InitContainers {

			if disabledInitContainers[container.Name] {
				log.G(h.Ctx).Infof("✅ [STATUS CALL] init container %s is marked as non-offloaded, reporting as Completed", container.Name)
				resp[i].InitContainers = append(resp[i].InitContainers, v1.ContainerStatus{
					Name:  container.Name,
					Ready: false,
					State: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{
							ExitCode: 0,
							Reason:   "Completed",
						},
					},
				})
				continue
			}

			containerName := podNamespace + "-" + podUID + "-" + container.Name
			cmd := []string{"exec " + podUID + "_dind" + " docker ps -af name=^" + containerName + "$ --format \"{{.Status}}\""}
			shell := exec.ExecTask{
				Command: "docker",
				Args:    cmd,
				Shell:   true,
			}
			execReturn, err := shell.Execute()
			execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

			if err != nil {
				log.G(h.Ctx).Error(err)
				statusCode = http.StatusInternalServerError
				break
			}

			log.G(h.Ctx).Info("\u2705 [STATUS CALL] Status of the init container retrieved successfully")

			initContainerStatus := strings.Split(execReturn.Stdout, " ")

			if execReturn.Stdout != "" {
				log.G(h.Ctx).Info("\u2705 [STATUS CALL] The container " + container.Name + " is in the state: " + initContainerStatus[0])

				if initContainerStatus[0] == "Created" {
					resp[i].InitContainers = append(resp[i].InitContainers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}}, Ready: false})
				} else if initContainerStatus[0] == "Up" {
					resp[i].InitContainers = append(resp[i].InitContainers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Running: &v1.ContainerStateRunning{}}, Ready: true})
				} else if initContainerStatus[0] == "Exited" {
					containerExitCode := strings.Split(initContainerStatus[1], "(")
					exitCode, err := strconv.Atoi(strings.Trim(containerExitCode[1], ")"))
					if err != nil {
						log.G(h.Ctx).Error(err)
						exitCode = 0
					}
					resp[i].InitContainers = append(resp[i].InitContainers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: int32(exitCode)}}, Ready: false})
				}
			} else {
				resp[i].InitContainers = append(resp[i].InitContainers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}}, Ready: false})
			}
		}

		disabledContainers := make(map[string]bool)
		if ann, ok := pod.Annotations["interlink.eu/disable-offload-containers"]; ok {
			for _, name := range strings.Split(ann, ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					disabledContainers[name] = true
				}
			}
		}

		for _, container := range pod.Spec.Containers {

			if disabledContainers[container.Name] {
				log.G(h.Ctx).Infof("✅ [STATUS CALL] container %s is marked as non-offloaded, reporting as Running", container.Name)
				resp[i].Containers = append(resp[i].Containers, v1.ContainerStatus{
					Name:  container.Name,
					Ready: true,
					State: v1.ContainerState{
						Running: &v1.ContainerStateRunning{},
					},
				})
				continue
			}

			containerName := podNamespace + "-" + podUID + "-" + container.Name
			cmd := []string{"exec " + podUID + "_dind" + " docker ps -af name=^" + containerName + "$ --format \"{{.Status}}\""}

			shell := exec.ExecTask{
				Command: "docker",
				Args:    cmd,
				Shell:   true,
			}
			execReturn, err := shell.Execute()
			execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

			if err != nil {
				log.G(h.Ctx).Error(err)
				statusCode = http.StatusInternalServerError
				break
			}

			log.G(h.Ctx).Info("\u2705 [STATUS CALL] Status of the container retrieved successfully")

			containerstatus := strings.Split(execReturn.Stdout, " ")

			if execReturn.Stdout != "" {
				log.G(h.Ctx).Info("\u2705 [STATUS CALL] The container " + container.Name + " is in the state: " + containerstatus[0])

				if containerstatus[0] == "Created" {
					resp[i].Containers = append(resp[i].Containers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}}, Ready: false})
				} else if containerstatus[0] == "Up" {
					resp[i].Containers = append(resp[i].Containers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Running: &v1.ContainerStateRunning{}}, Ready: true})
				} else if containerstatus[0] == "Exited" {
					containerExitCode := strings.Split(containerstatus[1], "(")
					exitCode, err := strconv.Atoi(strings.Trim(containerExitCode[1], ")"))
					if err != nil {
						log.G(h.Ctx).Error(err)
						exitCode = 0
					}
					resp[i].Containers = append(resp[i].Containers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: int32(exitCode)}}, Ready: false})
				}
			} else {
				resp[i].Containers = append(resp[i].Containers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}}, Ready: false})
			}
		}
	}

	w.WriteHeader(statusCode)

	if statusCode != http.StatusOK {
		w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
	} else {
		bodyBytes, err = json.Marshal(resp)
		if err != nil {
			log.G(h.Ctx).Error(err)
			statusCode = http.StatusInternalServerError
			w.WriteHeader(statusCode)
			w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
		}
		w.Write(bodyBytes)
	}

	if err != nil {
		span.SetAttributes(attribute.String("error", err.Error()))
	}
	commonIL.SetDurationSpan(start, span, commonIL.WithHTTPReturnCode(statusCode))
	span.End()

}
