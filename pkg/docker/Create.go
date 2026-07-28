package docker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"

	"errors"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"

	"path/filepath"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

func (h *SidecarHandler) prepareDockerRuns(podData commonIL.RetrievedPodData, w http.ResponseWriter) ([]DockerRunStruct, error) {

	var dockerRunStructs []DockerRunStruct
	var fpgaArgs string = ""

	podUID := string(podData.Pod.UID)
	podNamespace := string(podData.Pod.Namespace)

	pathsOfVolumes := make(map[string]string)

	for _, volume := range podData.Pod.Spec.Volumes {
		if volume.HostPath != nil {
			if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate || *volume.HostPath.Type == v1.HostPathDirectory {
				_, err := os.Stat(volume.HostPath.Path)
				if *volume.HostPath.Type == v1.HostPathDirectory {
					if os.IsNotExist(err) {
						HandleErrorAndRemoveData(h, w, "Host path directory does not exist", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("Host path directory does not exist")
					}
					pathsOfVolumes[volume.Name] = volume.HostPath.Path
				} else if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate {
					if os.IsNotExist(err) {
						err = os.MkdirAll(volume.HostPath.Path, os.ModePerm)
						if err != nil {
							HandleErrorAndRemoveData(h, w, "An error occurred during mkdir of host path directory", err, podNamespace, podUID)
							return dockerRunStructs, errors.New("An error occurred during mkdir of host path directory")
						} else {
							pathsOfVolumes[volume.Name] = volume.HostPath.Path
						}
					} else {
						pathsOfVolumes[volume.Name] = volume.HostPath.Path
					}
				}
			}
		}

		if volume.PersistentVolumeClaim != nil {
			if _, ok := pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName]; !ok {
				// WIP: this is a temporary solution to mount CVMFS volumes for persistent volume claims case
				pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName] = "/cvmfs"
			}

		}
	}

	allContainers := map[string][]v1.Container{
		"initContainers": podData.Pod.Spec.InitContainers,
		"containers":     podData.Pod.Spec.Containers,
	}

	for containerType, containers := range allContainers {
		isInitContainer := containerType == "initContainers"

		var envVars string = ""

		for _, container := range containers {

			containerName := podNamespace + "-" + podUID + "-" + container.Name

			var isFPGARequested bool = false

			if val, ok := container.Resources.Limits["xilinx.com/fpga"]; ok {
				numFPGAsRequested := val.Value()
				if numFPGAsRequested == 0 {
					log.G(h.Ctx).Info("\u2705 Container " + containerName + " is not requesting a FPGA")
				} else {

					if h.FPGAManager == nil {
						log.G(h.Ctx).Error("\u274C [CREATE CALL] FPGA Manager is not initialized")
						HandleErrorAndRemoveData(h, w, "FPGA Manager is not initialized", errors.New("FPGA Manager is not initialized"), podNamespace, podUID)
						return dockerRunStructs, errors.New("FPGA Manager is not initialized")
					}

					isFPGARequested = true
					log.G(h.Ctx).Info("\u2705 Container " + containerName + " is requesting " + strconv.Itoa(int(numFPGAsRequested)) + " FPGA(s)")

					numFPGAsRequestedInt := int(numFPGAsRequested)
					_, err := h.FPGAManager.GetAvailableFPGAs(numFPGAsRequestedInt)
					log.G(h.Ctx).Info("\u2705 [CREATE CALL] Retrieved available FPGAs")
					if err != nil {
						// log the err
						log.G(h.Ctx).Error("\u274C [CREATE CALL] Error retrieving available FPGAs")
						HandleErrorAndRemoveData(h, w, "An error occurred during the request of available FPGAs", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("An error occurred during the request of available FPGAs")
					}

					log.G(h.Ctx).Info("\u2705 [CREATE CALL] ********* BEFORE Requested FPGAs are available")

					assignedFPGAs, err := h.FPGAManager.GetAndAssignAvailableFPGAs(numFPGAsRequestedInt, containerName)
					log.G(h.Ctx).Info("\u2705 [CREATE CALL] ********* AFTER Requested FPGAs are available")

					// log the assigned FPGAs
					log.G(h.Ctx).Info("\u2705 [CREATE CALL] Assigned FPGAs: ")
					for _, fpgaSpec := range assignedFPGAs {
						log.G(h.Ctx).Info("\u2705 [CREATE CALL] " + fpgaSpec.DeviceToMount)
					}
					if err != nil {
						log.G(h.Ctx).Error("\u274C [CREATE CALL] Error during request of get and assign of an available FPGA")
						HandleErrorAndRemoveData(h, w, "An error occurred during request of get and assign of an available FPGA", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("An error occurred during request of get and assign of an available FPGA")
					}
					for _, fpgaSpec := range assignedFPGAs {
						fpgaArgs += " --device=" + fpgaSpec.DeviceToMount + ":" + fpgaSpec.DeviceToMount
					}
				}
			}

			for _, envVar := range container.Env {
				if envVar.Value != "" {
					value := envVar.Value

					// If the value starts with a double quote followed by a bracket,
					// remove the outer double quotes before wrapping.
					if strings.HasPrefix(value, "\"[") && strings.HasSuffix(value, "]\"") {
						// Remove the first and last character.
						value = value[1 : len(value)-1]
					}

					// Now, if the value looks like a list (starts with '['), wrap it with single quotes.
					if strings.HasPrefix(value, "[") {
						envVars += " -e " + envVar.Name + "='" + value + "'"
					} else if strings.Contains(value, " ") {
						// For values containing spaces, wrap in double quotes.
						envVars += " -e " + envVar.Name + "=\"" + value + "\""
					} else {
						envVars += " -e " + envVar.Name + "=" + value
					}
				} else {
					envVars += " -e " + envVar.Name
				}
			}

			for _, volumeMount := range container.VolumeMounts {
				if volumeMount.MountPath != "" {

					if _, ok := pathsOfVolumes[volumeMount.Name]; !ok {
						continue
					}
					if volumeMount.ReadOnly {
						envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath + ":ro"
					} else {
						if volumeMount.MountPropagation != nil && *volumeMount.MountPropagation == v1.MountPropagationBidirectional {
							envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath + ":shared"
						} else {
							envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath
						}
					}
				}
			}

			// if FPGA is requested, mount in read mode the Xilinx tools path in the container
			if isFPGARequested {
				envVars += " -v " + h.Config.XilinxToolsPath + ":" + h.Config.XilinxToolsPath + ":ro"
			}

			log.G(h.Ctx).Info("\u2705 [POD FLOW] Before creating run command")

			//envVars += " --network=host"
			cmd := []string{"run", "--user", "root", "-d", "--name", containerName}

			cmd = append(cmd, envVars)

			if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
				cmd = append(cmd, "--privileged")
			}

			if isFPGARequested {
				cmd = append(cmd, fpgaArgs)
			}

			var additionalPortArgs []string

			for _, port := range container.Ports {
				log.G(h.Ctx).Info("\u2705 [POD FLOW] Container port: " + strconv.Itoa(int(port.ContainerPort)) + " Protocol: " + string(port.Protocol) + " HostPort: " + strconv.Itoa(int(port.HostPort)))
				additionalPortArgs = append(additionalPortArgs, "-p", strconv.Itoa(int(port.ContainerPort))+":"+strconv.Itoa(int(port.ContainerPort)))
			}

			log.G(h.Ctx).Info("\u2705 [POD FLOW] Additional port arguments for container " + containerName + ": " + strings.Join(additionalPortArgs, " "))

			cmd = append(cmd, additionalPortArgs...)

			mounts, err := prepareMounts(h.Ctx, h.Config, podData, container)
			if err != nil {
				HandleErrorAndRemoveData(h, w, "An error occurred during preparing mounts for the POD", err, podNamespace, podUID)
				return dockerRunStructs, errors.New("An error occurred during preparing mounts for the POD")
			}

			cmd = append(cmd, mounts)

			memoryLimitsArray := []string{}
			cpuLimitsArray := []string{}

			if container.Resources.Limits.Memory().Value() != 0 {
				memoryLimitsArray = append(memoryLimitsArray, "--memory", strconv.Itoa(int(container.Resources.Limits.Memory().Value()))+"b")
			}
			if container.Resources.Limits.Cpu().Value() != 0 {
				cpuLimitsArray = append(cpuLimitsArray, "--cpus", strconv.FormatFloat(float64(container.Resources.Limits.Cpu().Value()), 'f', -1, 64))
			}

			cmd = append(cmd, memoryLimitsArray...)
			cmd = append(cmd, cpuLimitsArray...)

			containerCommands := []string{}
			containerArgs := []string{}
			mountFileCommand := []string{}

			// if container has a command and args, call parseContainerCommandAndReturnArgs
			if len(container.Command) > 0 || len(container.Args) > 0 {
				mountFileCommand, containerCommands, containerArgs, err = parseContainerCommandAndReturnArgs(h.Ctx, h.Config, podUID, podNamespace, container)
				if err != nil {
					HandleErrorAndRemoveData(h, w, "An error occurred during the parse of the container commands and arguments", err, podNamespace, podUID)
					return dockerRunStructs, errors.New("An error occurred during the parse of the container commands and arguments")
				}
				cmd = append(cmd, mountFileCommand...)
			}

			cmd = append(cmd, container.Image)
			cmd = append(cmd, containerCommands...)
			cmd = append(cmd, containerArgs...)

			dockerOptions := ""

			if dockerFlags, ok := podData.Pod.ObjectMeta.Annotations["docker-options.vk.io/flags"]; ok {
				parsedDockerOptions := strings.Split(dockerFlags, " ")
				for _, option := range parsedDockerOptions {
					dockerOptions += " " + option
				}
			}

			shell := exec.ExecTask{
				Command: "docker" + dockerOptions,
				Args:    cmd,
				Shell:   true,
			}

			dockerRunStructs = append(dockerRunStructs, DockerRunStruct{
				Name:            containerName,
				Command:         "docker " + strings.Join(shell.Args, " "),
				IsInitContainer: isInitContainer,
				FpgaArgs:        fpgaArgs,
			})
		}
	}

	return dockerRunStructs, nil
}

func (h *SidecarHandler) CreateHandler(w http.ResponseWriter, r *http.Request) {

	log.G(h.Ctx).Info("\u23F3 [CREATE CALL] Received create call from InterLink ")

	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	_, span := tracer.Start(h.Ctx, "Create", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))

	statusCode := http.StatusOK

	// Read and parse the request body FIRST, so the pod UID is known before a DIND
	// container is claimed. Claiming with the pod UID (ClaimAvailableDind) makes
	// the assignment atomic \u2014 Available=false and PodUID are set together \u2014 so a
	// failure at any later point can always find and clean up the right DIND.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during read of body request for pod creation", err, "", "")
		return
	}

	var req commonIL.RetrievedPodData
	if err = json.Unmarshal(bodyBytes, &req); err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during json unmarshal of data from pod creation request", err, "", "")
		return
	}

	log.G(h.Ctx).Info("\u2705 [POD FLOW] Request data unmarshalled successfully")

	podNamespace := string(req.Pod.Namespace)
	podUID := string(req.Pod.UID)

	// Atomically claim a DIND container for this pod.
	newDindContainerCreated := false
	dindContainerID, err := h.DindManager.ClaimAvailableDind(podUID)
	if err != nil {

		log.G(h.Ctx).Info("\u2705 [POD FLOW] No available DIND container found, creating a new one")

		// The build error must be reported: without it the only thing reaching the
		// caller is the generic "no available DIND container" from the claim below.
		if buildErr := h.DindManager.BuildDindContainers(1); buildErr != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the creation of a new DIND container", buildErr, podNamespace, podUID)
			return
		}
		dindContainerID, err = h.DindManager.ClaimAvailableDind(podUID)
		if err != nil {
			HandleErrorAndRemoveData(h, w, "During creation of new DIND container, an error occurred during the request of get available DIND container", err, podNamespace, podUID)
			return
		}
		newDindContainerCreated = true
	}

	if !newDindContainerCreated {
		// replenish the pool in the background since we consumed a pre-warmed one
		go h.DindManager.BuildDindContainers(1)
	}

	var newReq []commonIL.RetrievedPodData
	newReq = []commonIL.RetrievedPodData{req}

	for _, data := range newReq {

		podUID := string(data.Pod.UID)
		podNamespace := string(data.Pod.Namespace)

		podDirectoryPath := filepath.Join(h.Config.DataRootFolder, podNamespace+"-"+podUID)

		// Sentinel file written by mesh.sh once network setup is complete.
		// containers_command.sh polls for this file before starting workload containers.
		meshReadyFile := filepath.Join(podDirectoryPath, "mesh_ready")

		// log the pod specifics
		log.G(h.Ctx).Info(fmt.Sprintf("\u2705 [POD FLOW] Pod specs: %+v", data.Pod))

		annotations := make([]string, 0, len(data.Pod.Annotations))
		for key, value := range data.Pod.Annotations {
			annotations = append(annotations, key+"="+value)
		}
		log.G(h.Ctx).Info("\u2705 [POD FLOW] Pod Annotations are: " + strings.Join(annotations, ", "))

		// if the podDirectoryPath does not exist, create it
		if _, err := os.Stat(podDirectoryPath); os.IsNotExist(err) {
			err = os.MkdirAll(podDirectoryPath, os.ModePerm)
			if err != nil {
				HandleErrorAndRemoveData(h, w, "An error occurred during the creation of the pod directory", err, podNamespace, podUID)
				return
			}
		}

		// call prepareDockerRuns to get the DockerRunStruct array
		dockerRunStructs, err := h.prepareDockerRuns(data, w)
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during preparing of docker run commmands", err, podNamespace, podUID)
			return
		}

		if preExecAnnotations, ok := data.Pod.Annotations["slurm-job.vk.io/pre-exec"]; ok {
			if strings.Contains(preExecAnnotations, "cat <<'EOFMESH' > $TMPDIR/mesh.sh") {
				meshScript, err := extractHeredoc(preExecAnnotations, "EOFMESH")
				if err == nil && meshScript != "" {
					log.G(h.Ctx).Info("✅ [POD FLOW] Mesh.sh script extracted from annotation")

					// Extract the binary download section (before EOFSLIRP)
					downloadSection := extractDownloadSection(meshScript)

					// Extract the WG_IFACE variable definition from outer script
					wgIfaceDefinition := extractWGIfaceDefinition(meshScript)

					// Extract the WireGuard config section
					wgConfigSection := extractWGConfigSection(meshScript)

					// Extract just the inner script (EOFSLIRP content)
					innerScript, err := extractHeredoc(meshScript, "EOFSLIRP")
					if err == nil && innerScript != "" {
						finalDNSNameserver, finalDNSSearch := extractFinalDNSConfig(innerScript)

						// Store these for DIND configuration
						h.FinalDNSNameserver = finalDNSNameserver
						h.FinalDNSSearch = finalDNSSearch

						// Clean the header from inner script (remove duplicate shebang, set commands, etc.)
						innerScript = cleanInnerScriptHeader(innerScript)

						// Remove the final DNS config from inner script (we'll apply it to DIND)
						innerScript = removeFinalDNSConfig(innerScript)

						// Remove WG_IFACE definition from inner script (it's already extracted)
						innerScript = removeWGIfaceDefinition(innerScript)

						// Remove the slirp4netns execution at the end of inner script
						innerScript = removeSlirp4netnsExecution(innerScript)

						// Replace command execution with a sentinel touch followed by sleep infinity.
						// The sentinel file signals to containers_command.sh that network setup is done.
						innerScript = strings.Replace(innerScript, "$@", "touch "+meshReadyFile+" && sleep infinity", -1)

						// Build the complete script with correct order
						meshScript = `#!/bin/bash
set -e
set -m

sleep 20s

export PATH=$PATH:$PWD:/usr/sbin:/sbin

# Set up temporary directory
TMPDIR=${SLIRP_TMPDIR:-/tmp/.slirp.$RANDOM$RANDOM}
mkdir -p $TMPDIR
cd $TMPDIR

` + downloadSection + `

` + wgIfaceDefinition + `

` + wgConfigSection + `

` + innerScript
					} else {
						// Fallback: just clean up the outer script, still touch the sentinel before sleeping.
						meshScript = removeSlirp4netnsDownload(meshScript)
						meshScript = removeUnshareWrapper(meshScript)
						meshScript = strings.Replace(meshScript, "$@", "touch "+meshReadyFile+" && sleep infinity", -1)
						meshScript = removeSlirp4netnsExecution(meshScript)
					}

					log.G(h.Ctx).Info("✅ [POD FLOW] Mesh.sh script cleaned and simplified")

					// Create a special network overlay container name
					networkContainerName := podNamespace + "-" + podUID + "-network-overlay"

					// Save the modified mesh script to the pod directory
					meshScriptPath := filepath.Join(podDirectoryPath, "mesh.sh")
					err = os.WriteFile(meshScriptPath, []byte(meshScript), 0755)
					if err != nil {
						HandleErrorAndRemoveData(h, w, "An error occurred during the creation of mesh.sh script", err, podNamespace, podUID)
						return
					}

					log.G(h.Ctx).Info("✅ [POD FLOW] Mesh.sh script saved to " + meshScriptPath)

					// Prepare docker run command for the network overlay container
					networkCmd := []string{
						"run",
						"--user", "root",
						"-d",
						"--name", networkContainerName,
						"--privileged",
						"--cap-add", "NET_ADMIN",
						"--cap-add", "SYS_ADMIN",
						"--cap-add", "NET_RAW",
						"-v", meshScriptPath + ":/mesh.sh:ro",
						"-v", podDirectoryPath + ":" + podDirectoryPath,
						"-v", "/tmp:/tmp",
						"--network", "host",
						"nicolaka/netshoot",
						"/bin/bash", "/mesh.sh",
					}

					// Prepend this as the first init container
					dockerRunStructs = append([]DockerRunStruct{{
						Name:            networkContainerName,
						Command:         "docker " + strings.Join(networkCmd, " "),
						IsInitContainer: false,
						FpgaArgs:        "",
					}}, dockerRunStructs...)

					log.G(h.Ctx).Info("✅ [POD FLOW] Network overlay container prepared: " + networkContainerName)
				} else {
					log.G(h.Ctx).Error("❌ [POD FLOW] Failed to extract mesh.sh script from annotation")
					if err != nil {
						HandleErrorAndRemoveData(h, w, "Failed to extract mesh.sh heredoc", err, podNamespace, podUID)
						return
					}
				}
			}
		}

		log.G(h.Ctx).Info("\u2705 [POD FLOW] Docker run commands prepared successfully")

		// from dockerRunStructs, create two arrays: one for initContainers and one for containers
		var initContainers []DockerRunStruct
		var containers []DockerRunStruct
		//var gpuArgs string

		for _, dockerRunStruct := range dockerRunStructs {
			if dockerRunStruct.IsInitContainer {
				initContainers = append(initContainers, dockerRunStruct)
			} else {
				containers = append(containers, dockerRunStruct)
			}
		}

		// run the docker command to rename the container to the pod UID
		shell := exec.ExecTask{
			Command: "docker",
			Args:    []string{"rename", dindContainerID, string(data.Pod.UID) + "_dind"},
			Shell:   true,
		}

		_, err = shell.Execute()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the rename of the DIND container", err, podNamespace, podUID)
			return
		}

		isMeshScriptPresent := false

		// Configure DNS on the DIND container if mesh script is present
		if preExecAnnotations, ok := data.Pod.Annotations["slurm-job.vk.io/pre-exec"]; ok {
			if strings.Contains(preExecAnnotations, "cat <<'EOFMESH'") {
				log.G(h.Ctx).Info("✅ [POD FLOW] Configuring DNS on DIND container for cluster connectivity")

				isMeshScriptPresent = true

				// Use the extracted final DNS configuration
				dnsNameserver := h.FinalDNSNameserver
				dnsSearch := h.FinalDNSSearch

				if dnsNameserver == "" {
					dnsNameserver = "10.96.0.10" // Fallback
				}
				if dnsSearch == "" {
					dnsSearch = "default.svc.cluster.local svc.cluster.local cluster.local" // Fallback
				}

				// Create DNS configuration script for DIND
				/* 				dnsConfigScript := `#!/bin/sh
				set -e

				# Backup original resolv.conf
				cp /etc/resolv.conf /etc/resolv.conf.backup 2>/dev/null || true

				# Create new resolv.conf with cluster DNS
				cat > /etc/resolv.conf << EOF
				nameserver 8.8.8.8
				search ` + dnsSearch + `
				EOF

				echo "DNS configured for cluster connectivity"
				`

								// Write DNS config script to pod directory
								dnsScriptPath := filepath.Join(podDirectoryPath, "configure-dns.sh")
								err = os.WriteFile(dnsScriptPath, []byte(dnsConfigScript), 0755)
								if err != nil {
									log.G(h.Ctx).Warning("⚠️  Failed to create DNS config script: " + err.Error())
								} else {
									// Execute DNS configuration on DIND container
									dnsExecCmd := exec.ExecTask{
										Command: "docker",
										Args:    []string{"exec", string(data.Pod.UID) + "_dind", "sh", dnsScriptPath},
										Shell:   true,
									}
									_, err = dnsExecCmd.Execute()
									if err != nil {
										log.G(h.Ctx).Warning("⚠️  Failed to configure DNS on DIND container: " + err.Error())
									} else {
										log.G(h.Ctx).Info("✅ [POD FLOW] DNS configured on DIND container successfully with NS: " + dnsNameserver + ", Search: " + dnsSearch)
									}
								} */
			}
		}

		createResponse := CreateStruct{PodUID: string(data.Pod.UID), PodJID: dindContainerID}
		createResponseBytes, err := json.Marshal(createResponse)
		if err != nil {
			statusCode = http.StatusInternalServerError
			HandleErrorAndRemoveData(h, w, "An error occurred during the json marshal of the returned JID", err, podNamespace, podUID)
			return
		}

		span.SetAttributes(attribute.String("podUID", string(data.Pod.UID)))
		span.SetAttributes(attribute.String("podJID", dindContainerID))
		span.SetAttributes(attribute.String("podNamespace", string(data.Pod.Namespace)))
		span.SetAttributes(attribute.String("podName", string(data.Pod.Name)))

		w.WriteHeader(statusCode)

		if statusCode != http.StatusOK {
			w.Write([]byte("Some errors occurred while creating containers. Check Docker Sidecar's logs"))
		} else {
			w.Write(createResponseBytes)
		}

		if err != nil {
			span.SetAttributes(attribute.String("error", err.Error()))
		}
		commonIL.SetDurationSpan(start, span, commonIL.WithHTTPReturnCode(statusCode))
		span.End()

		go func() {

			if len(initContainers) > 0 {

				log.G(h.Ctx).Info("\u2705 [POD FLOW] Start creating init containers")

				// Create a list to hold the docker run commands
				var initContainerCommands []string

				// Build the docker run commands for each init container
				for _, initContainer := range initContainers {
					initContainerCommands = append(initContainerCommands, initContainer.Command+"\n")
				}

				// Log the init container commands
				log.G(h.Ctx).Info("\u2705 [POD FLOW] Init containers command list: " + strings.Join(initContainerCommands, ", "))

				// Run init containers sequentially
				for _, initContainer := range initContainers {
					log.G(h.Ctx).Info("\u2705 [POD FLOW] Executing init container: " + initContainer.Name)

					// Execute the docker command for the current init container
					shell := exec.ExecTask{
						Command: "docker",
						Args:    []string{"exec", string(data.Pod.UID) + "_dind", "/bin/sh", "-c", initContainer.Command},
					}

					_, err := shell.Execute()
					if err != nil {
						HandleErrorAndRemoveData(h, w, "An error occurred during the exec of the init container command", err, podNamespace, podUID)
						return
					}

					// Poll the container status until it exits
					for {
						shell = exec.ExecTask{
							Command: "docker",
							Args:    []string{"exec", string(data.Pod.UID) + "_dind", "docker", "inspect", "--format='{{.State.Status}}'", initContainer.Name},
						}

						statusReturn, err := shell.Execute()
						if err != nil {
							HandleErrorAndRemoveData(h, w, "An error occurred during inspect of init container", err, podNamespace, podUID)
							return
						}

						status := strings.Trim(statusReturn.Stdout, "'\n")
						if status == "exited" {
							log.G(h.Ctx).Info("\u2705 [POD FLOW] Init container " + initContainer.Name + " has completed")
							break
						} else {
							time.Sleep(1 * time.Second) // Wait for a second before polling again
						}
					}
				}

				log.G(h.Ctx).Info("\u2705 [POD FLOW] All init containers created and executed successfully")
			}

			log.G(h.Ctx).Info("\u2705 [POD FLOW] Start creating containers")

			// create a file called containers_command.sh and write the containers commands to it, use WriteFile function
			containersCommand := "#!/bin/sh\n"

			if isMeshScriptPresent {

				dnsNameserver := h.FinalDNSNameserver
				dnsSearch := h.FinalDNSSearch

				if dnsNameserver == "" {
					dnsNameserver = "10.96.0.10" // Fallback
				}
				if dnsSearch == "" {
					dnsSearch = "default.svc.cluster.local svc.cluster.local cluster.local" // Fallback
				}

				// The first container in the list is the network-overlay; start it immediately.
				// All subsequent containers are workload containers and must wait for the sentinel.
				networkOverlay := containers[0]
				workloadContainers := containers[1:]

				containersCommand += "# Start network overlay container first\n"
				containersCommand += networkOverlay.Command + "\n\n"

				// Poll for the sentinel file written by mesh.sh once network setup is complete.
				containersCommand += "echo 'Waiting for network overlay to be ready...'\n"
				containersCommand += "while [ ! -f " + meshReadyFile + " ]; do\n"
				containersCommand += "  echo 'Network not ready yet, waiting 2s...'\n"
				containersCommand += "  sleep 2\n"
				containersCommand += "done\n"
				containersCommand += "echo 'Network overlay is ready (sentinel file found), starting containers...'\n\n"

				for _, container := range workloadContainers {
					containersCommand += "# Start container: " + container.Name + "\n"
					containersCommand += container.Command + "\n"
					containersCommand += "sleep 2\n"

					// Configure DNS inside the container
					containersCommand += "# Configure DNS for container: " + container.Name + "\n"
					containersCommand += "docker exec " + container.Name + " sh -c '\n"
					containersCommand += "cp /etc/resolv.conf /etc/resolv.conf.backup 2>/dev/null || true\n"
					containersCommand += "cat > /etc/resolv.conf << EOF\n"
					containersCommand += "nameserver " + dnsNameserver + "\n"
					containersCommand += "nameserver 8.8.8.8 \n"
					containersCommand += "search " + dnsSearch + "\n"
					containersCommand += "EOF\n"
					containersCommand += "' || echo 'Warning: Could not configure DNS for " + container.Name + "'\n"
					containersCommand += "echo 'DNS configured for container: " + container.Name + "'\n\n"
				}
			} else {
				for _, container := range containers {
					containersCommand += container.Command + "\n"
					containersCommand += "sleep 1\n"
				}
			}
			err = os.WriteFile(podDirectoryPath+"/containers_command.sh", []byte(containersCommand), 0644)
			if err != nil {
				log.G(h.Ctx).Error("\u274C [POD FLOW] Error writing containers command script: " + err.Error())
				HandleErrorAndRemoveData(h, w, "An error occurred during the creation of the container commands script.", err, podNamespace, podUID)
				return
			}

			log.G(h.Ctx).Info("\u2705 [POD FLOW] Containers commands written to the script file")

			shell = exec.ExecTask{
				Command: "docker",
				Args:    []string{"exec", string(data.Pod.UID) + "_dind", "/bin/sh", podDirectoryPath + "/containers_command.sh"},
			}

			log.G(h.Ctx).Info("\u2705 [POD FLOW] Executing containers creation script inside DIND container; command to execute: docker " + strings.Join(shell.Args, " "))

			_, err = shell.Execute()
			if err != nil {
				log.G(h.Ctx).Error("\u274C [POD FLOW] Error executing containers command script: " + err.Error())
				HandleErrorAndRemoveData(h, w, "An error occurred during the execution of the container command script", err, podNamespace, podUID)
				return
			}

			log.G(h.Ctx).Info("\u2705 [POD FLOW] Containers created successfully")
		}()

	}

}

func HandleErrorAndRemoveData(h *SidecarHandler, w http.ResponseWriter, s string, err error, podNamespace string, podUID string) {
	log.G(h.Ctx).Error(err)
	log.G(h.Ctx).Info("\u274C Error description: " + s)
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte("Some errors occurred while creating container. Check Docker Sidecar's logs"))

	if podNamespace != "" && podUID != "" {
		os.RemoveAll(h.Config.DataRootFolder + podNamespace + "-" + podUID)
	}

	// Without a pod UID there is no DIND to reconcile (e.g. the body was never
	// parsed). Bail out here rather than matching an unrelated DIND by "".
	if podUID == "" {
		return
	}

	dindSpec, lookupErr := h.DindManager.GetDindFromPodUID(podUID)
	if lookupErr != nil {
		log.G(h.Ctx).Info("\u2139\uFE0F  [CREATE CALL] No DIND container assigned to pod " + podUID + " to clean up")
		return
	}

	log.G(h.Ctx).Info("\u2705 [CREATE CALL] Cleaning up DIND for pod " + podUID + ": " + dindSpec.DindID + " (network " + dindSpec.DindNetworkID + ")")

	// Force-remove the DIND container FIRST. While it is running it holds an
	// endpoint on its bridge network, so the network cannot be removed and its
	// /24 subnet stays effectively allocated \u2014 which then causes the next create
	// drawing that subnet from the pool to fail with an address overlap. The
	// container may be named after the pod UID (after the rename step) or after
	// its original build UID (before it); remove both, ignoring "no such
	// container" errors.
	for _, name := range []string{podUID + "_dind", dindSpec.DindID} {
		if name == "" {
			continue
		}
		rm := exec.ExecTask{Command: "docker", Args: []string{"rm", "-f", name}, Shell: true}
		if execReturn, rmErr := rm.Execute(); rmErr != nil || execReturn.ExitCode != 0 {
			log.G(h.Ctx).Warning("\u26A0\uFE0F  [CREATE CALL] Could not remove DIND container " + name + ": " + strings.TrimSpace(execReturn.Stderr))
		}
	}

	// Now that the container is gone, RemoveDindFromList can actually delete the
	// bridge network and return the subnet to the pool.
	if remErr := h.DindManager.RemoveDindFromList(dindSpec.PodUID); remErr != nil {
		log.G(h.Ctx).Error("\u274C [CREATE CALL] Error removing DIND from list: " + remErr.Error())
	}
}
