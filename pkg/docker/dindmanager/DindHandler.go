package dindmanager

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"

	OSexec "os/exec"
)

type DindManagerInterface interface {
	CleanDindContainers() error
	BuildDindContainers(nDindContainer int8) error
	PrintDindList() error
	GetAvailableDind() (string, error)
	SetDindUnavailable(dindID string) error
	RemoveDindFromList(PodUID string) error
	SetPodUIDToDind(dindID string, podUID string) error
	GetDindFromPodUID(podUID string) (DindSpecs, error)
	SetDindAvailable(PodUID string) error
}

type DindSpecs struct {
	DindID          string
	PodUID          string
	DindNetworkID   string
	AllocatedSubnet string // empty if no subnet pool was configured
	Available       bool
}

type DindManager struct {
	DindList        []DindSpecs
	Ctx             context.Context
	FPGAEnabled     bool
	XilinxToolsPath string

	// Subnet pool: pre-generated /24 subnets from the parent CIDRs in config.
	// Protected by mu because container creation/deletion can happen concurrently.
	SubnetPool    []string
	mu            sync.Mutex
	InitialPoolSz int // total subnets at startup, used for logging
}

// ---------------------------------------------------------------------------
// Subnet pool helpers
// ---------------------------------------------------------------------------

// InitSubnetPool expands a list of parent CIDRs (e.g. ["192.168.0.0/16",
// "10.12.0.0/16"]) into an ordered slice of allocatable /24 subnets.
// Supported parent prefix lengths: /8, /16, /24.
// Returns an empty slice (no error) when parentCIDRs is nil/empty.
func InitSubnetPool(parentCIDRs []string) ([]string, error) {
	var pool []string
	for _, cidr := range parentCIDRs {
		subnets, err := generateSubnetsFromCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("failed to expand CIDR %s: %w", cidr, err)
		}
		pool = append(pool, subnets...)
	}
	return pool, nil
}

// generateSubnetsFromCIDR returns all usable /24 subnets within parentCIDR.
//
//   - /8  → iterates second octet [1-254] × third octet [1-254]  (~64 k subnets)
//   - /16 → iterates third octet  [1-254]                        (254 subnets)
//   - /24 → returns the CIDR itself                              (1 subnet)
func generateSubnetsFromCIDR(cidr string) ([]string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}

	ones, bits := ipNet.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("only IPv4 CIDRs are supported (got %s)", cidr)
	}

	base := ipNet.IP.To4()
	var subnets []string

	switch ones {
	case 8:
		for second := 1; second <= 254; second++ {
			for third := 1; third <= 254; third++ {
				subnets = append(subnets, fmt.Sprintf("%d.%d.%d.0/24", base[0], second, third))
			}
		}
	case 16:
		for third := 1; third <= 254; third++ {
			subnets = append(subnets, fmt.Sprintf("%d.%d.%d.0/24", base[0], base[1], third))
		}
	case 24:
		subnets = append(subnets, cidr)
	default:
		return nil, fmt.Errorf("unsupported prefix length /%d in %s: only /8, /16 and /24 are supported", ones, cidr)
	}

	return subnets, nil
}

// allocateSubnet pops the next available subnet from the pool.
// Returns ("", false) when the pool is empty or was never configured.
func (a *DindManager) allocateSubnet() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.SubnetPool) == 0 {
		return "", false
	}
	subnet := a.SubnetPool[0]
	a.SubnetPool = a.SubnetPool[1:]
	return subnet, true
}

// freeSubnet returns a subnet to the pool (appended at the end so the pool
// stays ordered and previously-used ranges are re-used last).
func (a *DindManager) freeSubnet(subnet string) {
	if subnet == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.SubnetPool = append(a.SubnetPool, subnet)
}

// remainingSubnets returns the current pool size (thread-safe).
func (a *DindManager) remainingSubnets() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.SubnetPool)
}

// ---------------------------------------------------------------------------
// UUIDv4 generator (unchanged)
// ---------------------------------------------------------------------------

func GenerateUUIDv4() (string, error) {
	uuid := make([]byte, 16)
	_, err := rand.Read(uuid)
	if err != nil {
		return "", err
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40
	uuid[8] = (uuid[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16]), nil
}

// ---------------------------------------------------------------------------
// DindManager methods
// ---------------------------------------------------------------------------

func (a *DindManager) CleanDindContainers() error {
	log.G(a.Ctx).Info("\u2705 Start cleaning zombie DIND containers")

	shell := exec.ExecTask{
		Command: "docker",
		Args:    []string{"ps", "-a", "--format", "{{.Names}}", "|", "grep", "_dind$", "|", "wc", "-l"},
		Shell:   true,
	}
	execReturn, err := shell.Execute()
	if err != nil {
		return err
	}
	log.G(a.Ctx).Info(fmt.Sprintf("\u2705 %s zombie DIND containers found",
		strings.ReplaceAll(execReturn.Stdout, "\n", "")))

	shell = exec.ExecTask{
		Command: "docker",
		Args:    []string{"ps", "-a", "--format", "{{.Names}}", "|", "grep", "_dind$", "|", "xargs", "-I", "{}", "docker", "rm", "-f", "{}"},
		Shell:   true,
	}
	if _, err = shell.Execute(); err != nil {
		return err
	}

	shell = exec.ExecTask{
		Command: "docker",
		Args:    []string{"network", "ls", "--filter", "name=_dind_network$", "--format", "{{.ID}}", "|", "xargs", "-r", "docker", "network", "rm"},
		Shell:   true,
	}
	if _, err = shell.Execute(); err != nil {
		return err
	}

	log.G(a.Ctx).Info("\u2705 DIND zombie containers cleaned")
	return nil
}

func (a *DindManager) BuildDindContainers(nDindContainer int8) error {
	log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Creating %d DIND containers", nDindContainer))

	// Log subnet pool status before we start allocating.
	if a.InitialPoolSz > 0 {
		log.G(a.Ctx).Info(fmt.Sprintf(
			"\u2705 Subnet pool: %d/%d subnets available before container creation",
			a.remainingSubnets(), a.InitialPoolSz,
		))
	} else {
		log.G(a.Ctx).Info("\u2705 No subnet pool configured — networks will use Docker's automatic addressing")
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	gpuEnabled := os.Getenv("GPUENABLED")
	dindImage := "docker:dind"
	if gpuEnabled == "1" {
		dindImage = "ghcr.io/extrality/nvidia-dind"
	}

	for i := int8(0); i < nDindContainer; i++ {

		randUID, err := GenerateUUIDv4()
		if err != nil {
			return err
		}

		// ----------------------------------------------------------------
		// Allocate a /24 subnet for this container's bridge network (if a
		// pool is configured).
		// ----------------------------------------------------------------
		allocatedSubnet, hasSubnet := a.allocateSubnet()

		networkArgs := []string{"network", "create", "--driver", "bridge"}
		if hasSubnet {
			networkArgs = append(networkArgs, "--subnet", allocatedSubnet)
		}
		networkArgs = append(networkArgs, randUID+"_dind_network")

		shell := exec.ExecTask{
			Command: "docker",
			Args:    networkArgs,
			Shell:   true,
		}
		if _, err = shell.Execute(); err != nil {
			// Return the subnet to the pool so it is not lost on error.
			a.freeSubnet(allocatedSubnet)
			return err
		}

		if hasSubnet {
			log.G(a.Ctx).Info(fmt.Sprintf(
				"\u2705 DIND network %s created with subnet %s (%d/%d subnets remaining in pool)",
				randUID+"_dind_network", allocatedSubnet,
				a.remainingSubnets(), a.InitialPoolSz,
			))
		} else {
			log.G(a.Ctx).Info(fmt.Sprintf(
				"\u2705 DIND network %s created (no subnet pool configured)",
				randUID+"_dind_network",
			))
		}

		// ----------------------------------------------------------------
		// Build the docker-run argument list.
		// ----------------------------------------------------------------
		dindContainerArgs := []string{"run"}

		if _, err := os.Stat("/cvmfs"); err == nil {
			dindContainerArgs = append(dindContainerArgs, "-v", "/cvmfs:/cvmfs")
		}

		if a.FPGAEnabled {
			if _, err := os.Stat(a.XilinxToolsPath); err == nil {
				dindContainerArgs = append(dindContainerArgs, "-v", a.XilinxToolsPath+":"+a.XilinxToolsPath+":ro")
			}
		}

		dindContainerArgs = append(dindContainerArgs, "--network", randUID+"_dind_network")
		dindContainerArgs = append(dindContainerArgs, "--cap-add", "NET_ADMIN")

		if gpuEnabled == "1" {
			dindContainerArgs = append(dindContainerArgs, "--runtime=nvidia")
		}
		dindContainerArgs = append(dindContainerArgs,
			"--privileged",
			"-v", wd+":"+"/"+wd,
			"-v", "/home:/home",
			"-v", "/var/lib/docker/overlay2:/var/lib/docker/overlay2",
			"-v", "/var/lib/docker/image:/var/lib/docker/image",
			"-d", "--name", randUID+"_dind", dindImage,
		)

		shell = exec.ExecTask{
			Command: "docker",
			Args:    dindContainerArgs,
			Shell:   true,
		}
		log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Command is %s", shell.Command+" "+strings.Join(shell.Args, " ")))

		execReturn, err := shell.Execute()
		if err != nil {
			log.G(a.Ctx).Error(fmt.Sprintf("\u274c Error creating DIND container %s", randUID+"_dind"))
			log.G(a.Ctx).Error(fmt.Sprintf("\u274c %s", execReturn.Stderr))
			// Free the subnet so it can be reused.
			a.freeSubnet(allocatedSubnet)
			return err
		}
		dindContainerID := execReturn.Stdout

		// ----------------------------------------------------------------
		// Wait for the daemon inside the DinD container to be ready.
		// ----------------------------------------------------------------
		maxRetries := 20
		for {
			if maxRetries == 0 {
				a.freeSubnet(allocatedSubnet)
				return fmt.Errorf("DIND container %s did not become ready in time", dindContainerID)
			}

			cmd := OSexec.Command("docker", "logs", randUID+"_dind")
			output, err := cmd.CombinedOutput()
			if err == nil && strings.Contains(string(output), "API listen on /var/run/docker.sock") {
				break
			}
			time.Sleep(1 * time.Second)
			maxRetries--
		}
		log.G(a.Ctx).Info(fmt.Sprintf("\u2705 DIND container %s is up and running", dindContainerID))

		// ----------------------------------------------------------------
		// Install required tools inside the DinD container.
		// ----------------------------------------------------------------
		for _, pkg := range []string{"net-tools", "iproute2"} {
			shell = exec.ExecTask{
				Command: "docker",
				Args:    []string{"exec", randUID + "_dind", "apt-get", "install", "-y", pkg},
				Shell:   true,
			}
			if _, err = shell.Execute(); err != nil {
				a.freeSubnet(allocatedSubnet)
				return err
			}
			log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Installed %s", pkg))
		}

		// ----------------------------------------------------------------
		// Register the new DinD in the list.
		// ----------------------------------------------------------------
		a.DindList = append(a.DindList, DindSpecs{
			DindID:          randUID + "_dind",
			PodUID:          "",
			DindNetworkID:   randUID + "_dind_network",
			AllocatedSubnet: allocatedSubnet, // empty string when no pool
			Available:       true,
		})
	}

	return nil
}

func (a *DindManager) PrintDindList() error {
	for _, d := range a.DindList {
		log.G(a.Ctx).Info(fmt.Sprintf(
			"DindID: %s, PodUID: %s, DindNetworkID: %s, AllocatedSubnet: %q, Available: %t",
			d.DindID, d.PodUID, d.DindNetworkID, d.AllocatedSubnet, d.Available,
		))
	}
	return nil
}

func (a *DindManager) GetDindFromPodUID(podUID string) (DindSpecs, error) {
	for _, d := range a.DindList {
		if d.PodUID == podUID {
			return d, nil
		}
	}
	return DindSpecs{}, fmt.Errorf("DIND container with PodUID %s not found", podUID)
}

func (a *DindManager) GetAvailableDind() (string, error) {
	for _, d := range a.DindList {
		if d.Available {
			return d.DindID, nil
		}
	}
	return "", fmt.Errorf("no available DIND container")
}

func (a *DindManager) SetDindUnavailable(dindID string) error {
	for i, d := range a.DindList {
		if d.DindID == dindID {
			a.DindList[i].Available = false
			return nil
		}
	}
	return fmt.Errorf("DIND container %s not found", dindID)
}

func (a *DindManager) SetDindAvailable(PodUID string) error {
	for i, d := range a.DindList {
		if d.PodUID == PodUID {
			a.DindList[i].Available = true
			return nil
		}
	}
	return fmt.Errorf("DIND container with PodUID %s not found", PodUID)
}

func (a *DindManager) SetPodUIDToDind(dindID string, podUID string) error {
	for i, d := range a.DindList {
		if d.DindID == dindID {
			a.DindList[i].PodUID = podUID
			return nil
		}
	}
	return fmt.Errorf("DIND container %s not found", dindID)
}

// RemoveDindFromList removes the DinD entry associated with PodUID from the
// in-memory list, tears down its Docker network, and returns its subnet to
// the pool so it can be reused by future containers.
func (a *DindManager) RemoveDindFromList(PodUID string) error {
	for i, d := range a.DindList {
		if d.PodUID != PodUID {
			continue
		}

		// Remove the dedicated bridge network from Docker.
		if d.DindNetworkID != "" {
			shell := exec.ExecTask{
				Command: "docker",
				Args:    []string{"network", "rm", d.DindNetworkID},
				Shell:   true,
			}
			if execReturn, err := shell.Execute(); err != nil {
				// Log but don't abort — the container may already be gone.
				log.G(a.Ctx).Warn(fmt.Sprintf(
					"\u26a0\ufe0f  Could not remove network %s: %s (stderr: %s)",
					d.DindNetworkID, err, execReturn.Stderr,
				))
			} else {
				log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Removed Docker network %s", d.DindNetworkID))
			}
		}

		// Return the subnet to the pool.
		a.freeSubnet(d.AllocatedSubnet)
		if d.AllocatedSubnet != "" {
			log.G(a.Ctx).Info(fmt.Sprintf(
				"\u2705 Subnet %s returned to pool (%d/%d now available)",
				d.AllocatedSubnet, a.remainingSubnets(), a.InitialPoolSz,
			))
		}

		// Splice the entry out of the list.
		a.DindList = append(a.DindList[:i], a.DindList[i+1:]...)
		return nil
	}
	return fmt.Errorf("DIND container with PodUID %s not found", PodUID)
}
