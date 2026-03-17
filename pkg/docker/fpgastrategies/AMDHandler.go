package fpgastrategies

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"regexp"
	"sort"
	"strings"

	exec "github.com/alexellis/go-execute/pkg/v1"

	"sync"

	"github.com/containerd/containerd/log"
)

type FPGASpecs struct {
	BDF           string
	Shell         string
	LogicUUID     string
	deviceID      string
	ContainerID   string
	DeviceReady   string
	DeviceToMount string
	Available     bool
	Index         int
}

type FPGAManager struct {
	FPGASpecsList  []FPGASpecs
	FPGASpecsMutex sync.Mutex
	Vendor         string
	Ctx            context.Context
	VitisPath      string
	XRTPath        string
}

type FPGAManagerInterface interface {
	Init() error
	Shutdown() error
	GetFPGASpecsList() []FPGASpecs
	Dump() error
	Discover() error
	Check() error
	GetAvailableFPGAs(numFPGAs int) ([]FPGASpecs, error)
	Assign(BDF string, containerID string) error
	Release(UUID string) error
	GetAndAssignAvailableFPGAs(numFPGAs int, containerID string) ([]FPGASpecs, error)
}

func (a *FPGAManager) Init() error {

	// Check if the Xilinx setup.sh file exists
	if _, err := os.Stat(a.XRTPath + "/setup.sh"); os.IsNotExist(err) {
		return fmt.Errorf("%s/setup.sh does not exist: %v", a.XRTPath, err)
	}

	// Check if the path to Vitis exists
	if _, err := os.Stat(a.VitisPath); os.IsNotExist(err) {
		return fmt.Errorf("%s does not exist: %v", a.VitisPath, err)
	}

	// Source the setup.sh to initialize Xilinx tools using shell
	shellArgs := []string{"source", a.XRTPath + "/setup.sh"}
	shell := exec.ExecTask{
		Command: "/bin/bash",
		Args:    shellArgs,
		Shell:   true,
	}

	_, err := shell.Execute()
	if err != nil {
		return fmt.Errorf("Error running source setup.sh command: %v", err)
	}

	return nil
}

// Discover implements the Discover function of the FPGAManager interface
func (a *FPGAManager) Discover() error {

	shellArgs := []string{"|", "grep", "Xilinx"}

	shell := exec.ExecTask{
		Command: "/usr/bin/lspci",
		Args:    shellArgs,
		Shell:   true,
	}

	output, err := shell.Execute()
	if err != nil {
		return fmt.Errorf("Error running lspci command: %v", err)
	}

	lines := strings.Split(string(output.Stdout), "\n")
	xilinxFound := false
	for _, line := range lines {
		if strings.Contains(line, "Xilinx") {
			xilinxFound = true
			log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Found potential FPGA: %s", line))
			break
		}
	}

	if !xilinxFound {
		log.G(a.Ctx).Info("\u2705 No FPGAs discovered")
		return nil
	}

	// Source XRT setup.sh before running xbutil
	sourceShell := exec.ExecTask{
		Command: "source",
		Args:    []string{a.XRTPath + "/setup.sh"},
		Shell:   true,
	}
	_, err = sourceShell.Execute()
	if err != nil {
		return fmt.Errorf("Error running source setup.sh command: %v", err)
	}

	// Get a temp path but don't create the file — xbutil will create it
	tmpFile, err := os.CreateTemp("", "xbutil-examine-*.json")
	if err != nil {
		return fmt.Errorf("Error creating temp file for xbutil output: %v", err)
	}
	tmpFilePath := tmpFile.Name()
	tmpFile.Close()
	os.Remove(tmpFilePath) // Remove it so xbutil can write to it freely
	defer os.Remove(tmpFilePath)

	log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Running xbutil examine -f json -o %s", tmpFilePath))

	cmd := exec.ExecTask{
		Command: a.XRTPath + "/bin/xbutil",
		Args:    []string{"examine", "-f", "json", "-o", tmpFilePath},
		Shell:   false,
	}
	result, err := cmd.Execute()
	if err != nil {
		return fmt.Errorf("Error running xbutil examine: %v", err)
	}

	// Log stderr to help debug if JSON is still empty
	if result.Stderr != "" {
		log.G(a.Ctx).Info(fmt.Sprintf("xbutil stderr: %s", result.Stderr))
	}

	// Verify the file was actually written
	info, err := os.Stat(tmpFilePath)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("xbutil did not write output to %s (stderr: %s)", tmpFilePath, result.Stderr)
	}

	// Parse the JSON output
	jsonData, err := os.ReadFile(tmpFilePath)
	if err != nil {
		return fmt.Errorf("Error reading xbutil JSON output: %v", err)
	}

	var examineOutput struct {
		System struct {
			Host struct {
				Devices []struct {
					BDF      string `json:"bdf"`
					VBNV     string `json:"vbnv"`
					ID       string `json:"id"`
					Instance string `json:"instance"`
					IsReady  string `json:"is_ready"`
				} `json:"devices"`
			} `json:"host"`
		} `json:"system"`
	}

	if err := json.Unmarshal(jsonData, &examineOutput); err != nil {
		return fmt.Errorf("Error parsing xbutil JSON output: %v", err)
	}

	reDeviceID := regexp.MustCompile(`user\(inst=(\d+)\)`)

	for _, device := range examineOutput.System.Host.Devices {
		log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Processing device BDF: %s, VBNV: %s, ID: %s", device.BDF, device.VBNV, device.ID))

		// Check if already in the list
		alreadyFound := false
		for _, fpgaSpec := range a.FPGASpecsList {
			if fpgaSpec.BDF == device.BDF {
				alreadyFound = true
				break
			}
		}
		if alreadyFound {
			continue
		}

		deviceID := ""
		if matches := reDeviceID.FindStringSubmatch(device.Instance); len(matches) > 1 {
			deviceID = matches[1]
		}

		spec := FPGASpecs{
			BDF:           device.BDF,
			Shell:         device.VBNV,
			LogicUUID:     device.ID,
			deviceID:      deviceID,
			DeviceToMount: "/dev/dri/renderD" + deviceID,
			Available:     device.IsReady == "true",
		}
		a.FPGASpecsList = append(a.FPGASpecsList, spec)
	}

	if len(a.FPGASpecsList) > 0 {
		log.G(a.Ctx).Info("\u2705 Discovered FPGAs:")
		for _, fpgaSpec := range a.FPGASpecsList {
			log.G(a.Ctx).Info(fmt.Sprintf("\u2705 BDF: %s, Shell: %s, LogicUUID: %s, DeviceID: %s, Available: %t",
				fpgaSpec.BDF, fpgaSpec.Shell, fpgaSpec.LogicUUID, fpgaSpec.deviceID, fpgaSpec.Available))
		}
		log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Total FPGAs discovered: %d", len(a.FPGASpecsList)))
	} else {
		log.G(a.Ctx).Info("\u2705 No FPGAs discovered")
	}

	return nil
}

func (a *FPGAManager) Check() error {
	return nil
}

func (a *FPGAManager) Shutdown() error {

	return nil
}

func (a *FPGAManager) GetFPGASpecsList() []FPGASpecs {
	return a.FPGASpecsList
}

func (a *FPGAManager) Assign(BDF string, containerID string) error {
	for i := range a.FPGASpecsList {
		if a.FPGASpecsList[i].BDF == BDF {
			disableBookkeeping := os.Getenv("FPGA_DISABLE_BOOKKEEPING") == "1"
			if disableBookkeeping {
				a.FPGASpecsList[i].ContainerID = containerID
				a.FPGASpecsList[i].Available = false
				break
			}

			if !a.FPGASpecsList[i].Available {
				return fmt.Errorf("FPGA with BDF %s is already in use by container %s", BDF, a.FPGASpecsList[i].ContainerID)
			}

			a.FPGASpecsList[i].ContainerID = containerID
			a.FPGASpecsList[i].Available = false
			break
		}
	}
	return nil
}

func (a *FPGAManager) Release(containerID string) error {

	a.FPGASpecsMutex.Lock()
	defer a.FPGASpecsMutex.Unlock()

	for i := range a.FPGASpecsList {
		if a.FPGASpecsList[i].ContainerID == containerID {

			if a.FPGASpecsList[i].Available {
				continue
			}

			a.FPGASpecsList[i].ContainerID = ""
			a.FPGASpecsList[i].Available = true
		}
	}

	return nil
}

/* func (a *FPGAManager) GetAvailableFPGAs(numFPGAs int) ([]FPGASpecs, error) {
	a.FPGASpecsMutex.Lock()
	defer a.FPGASpecsMutex.Unlock()

	var availableFPGAs []FPGASpecs
	disableBookkeeping := os.Getenv("FPGA_DISABLE_BOOKKEEPING") == "1"

	if disableBookkeeping {
		fpgaUsage := make(map[string]int)
		for _, fpga := range a.FPGASpecsList {
			if fpga.ContainerID != "" {
				fpgaUsage[fpga.BDF]++
			} else {
				// If an unassigned FPGA is found, prioritize it
				availableFPGAs = append(availableFPGAs, fpga)
			}
		}

		if len(availableFPGAs) >= numFPGAs {
			return availableFPGAs[:numFPGAs], nil
		}

		// If not enough unassigned FPGAs, find the least assigned ones
		sort.Slice(a.FPGASpecsList, func(i, j int) bool {
			return fpgaUsage[a.FPGASpecsList[i].BDF] < fpgaUsage[a.FPGASpecsList[j].BDF]
		})

		for _, fpga := range a.FPGASpecsList {
			if len(availableFPGAs) < numFPGAs {
				availableFPGAs = append(availableFPGAs, fpga)
			} else {
				break
			}
		}

		if len(availableFPGAs) >= numFPGAs {
			return availableFPGAs[:numFPGAs], nil
		}

		return nil, fmt.Errorf("Not enough FPGAs available. Requested: %d, Found: %d", numFPGAs, len(availableFPGAs))
	}

	// Default behavior: return only available FPGAs
	for _, fpgaSpec := range a.FPGASpecsList {
		if fpgaSpec.Available {
			availableFPGAs = append(availableFPGAs, fpgaSpec)
			if len(availableFPGAs) == numFPGAs {
				return availableFPGAs, nil
			}
		}
	}

	return nil, fmt.Errorf("Not enough available FPGAs. Requested: %d, Available: %d", numFPGAs, len(availableFPGAs))
} */

func (a *FPGAManager) GetAvailableFPGAs(numFPGAs int) ([]FPGASpecs, error) {

	var availableFPGAs []FPGASpecs

	fmt.Println("Checking for available FPGAs")

	disableBookkeeping := os.Getenv("FPGA_DISABLE_BOOKKEEPING") == "1"

	fmt.Println(fmt.Sprintf("FPGA_DISABLE_BOOKKEEPING: %v", disableBookkeeping))

	if disableBookkeeping {
		fmt.Println("FPGA_DISABLE_BOOKKEEPING is set to 1. Disabling bookkeeping")

		fpgaUsage := make(map[string]int)
		for _, fpga := range a.FPGASpecsList {
			if fpga.ContainerID != "" {
				fpgaUsage[fpga.BDF]++
			} else {
				// If an unassigned FPGA is found, prioritize it
				availableFPGAs = append(availableFPGAs, fpga)
			}
		}

		fmt.Println(fmt.Sprintf("FPGA Usage: %v", fpgaUsage))

		if len(availableFPGAs) >= numFPGAs {
			return availableFPGAs[:numFPGAs], nil
		}

		// If not enough unassigned FPGAs, find the least assigned ones
		sort.Slice(a.FPGASpecsList, func(i, j int) bool {
			return fpgaUsage[a.FPGASpecsList[i].BDF] < fpgaUsage[a.FPGASpecsList[j].BDF]
		})

		for _, fpga := range a.FPGASpecsList {
			if len(availableFPGAs) < numFPGAs {
				availableFPGAs = append(availableFPGAs, fpga)
			} else {
				break
			}
		}

		if len(availableFPGAs) >= numFPGAs {
			return availableFPGAs[:numFPGAs], nil
		}

		return nil, fmt.Errorf("Not enough FPGAs available. Requested: %d, Found: %d", numFPGAs, len(availableFPGAs))
	}

	for _, fpgaSpec := range a.FPGASpecsList {
		if fpgaSpec.Available {
			availableFPGAs = append(availableFPGAs, fpgaSpec)
			if len(availableFPGAs) == numFPGAs {
				return availableFPGAs, nil
			}
		}
	}
	return nil, fmt.Errorf("Not enough available FPGAs. Requested: %d, Available: %d", numFPGAs, len(availableFPGAs))
}

func (a *FPGAManager) GetAndAssignAvailableFPGAs(numFPGAs int, containerID string) ([]FPGASpecs, error) {
	a.FPGASpecsMutex.Lock()
	defer a.FPGASpecsMutex.Unlock()

	fpgaSpecs, err := a.GetAvailableFPGAs(numFPGAs)
	if err != nil {
		return nil, err
	}

	for _, fpgaSpec := range fpgaSpecs {
		err = a.Assign(fpgaSpec.BDF, containerID) // ← was fpgaSpec.LogicUUID
		if err != nil {
			return nil, err
		}
	}

	return fpgaSpecs, nil
}

// dump the FPGASpecsList into a JSON file
func (a *FPGAManager) Dump() error {

	// Convert the array to JSON format
	jsonData, err := json.MarshalIndent(a.FPGASpecsList, "", "  ")
	if err != nil {
		return fmt.Errorf("Error marshalling JSON: %v", err)
	}

	// Write JSON data to a file
	err = ioutil.WriteFile("fpga_specs.json", jsonData, 0644)
	if err != nil {
		return fmt.Errorf("Error writing to file: %v", err)
	}

	return nil
}
