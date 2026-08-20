package common

import (
	"time"

	v1 "k8s.io/api/core/v1"
)

type DockerConfig struct {
	VKConfigPath              string   `yaml:"VKConfigPath"`
	Socket                    string   `yaml:"Socket"`
	ExportPodData             bool     `yaml:"ExportPodData"`
	Commandprefix             string   `yaml:"CommandPrefix"`
	ImagePrefix               string   `yaml:"ImagePrefix"`
	DataRootFolder            string   `yaml:"DataRootFolder"`
	Namespace                 string   `yaml:"Namespace"`
	Tsocks                    bool     `yaml:"Tsocks"`
	Tsockspath                string   `yaml:"TsocksPath"`
	Tsockslogin               string   `yaml:"TsocksLoginNode"`
	BashPath                  string   `yaml:"BashPath"`
	VerboseLogging            bool     `yaml:"VerboseLogging"`
	ErrorsOnlyLogging         bool     `yaml:"ErrorsOnlyLogging"`
	SingularityDefaultOptions []string `yaml:"SingularityDefaultOptions"`
	SingularityPrefix         string   `yaml:"SingularityPrefix"`
	SingularityPath           string   `yaml:"SingularityPath"`
	EnableProbes              bool     `yaml:"EnableProbes"`
	set                       bool
}

// PodCreateRequests is a struct holding data for a create request. Retrieved ConfigMaps and Secrets are held along the Pod description itself.
type PodCreateRequests struct {
	Pod        v1.Pod         `json:"pod"`
	ConfigMaps []v1.ConfigMap `json:"configmaps"`
	Secrets    []v1.Secret    `json:"secrets"`
}

// PodStatus is a simplified v1.Pod struct, holding only necessary variables to uniquely identify a job/service in the sidecar. It is used to request
type PodStatus struct {
	PodName        string               `json:"name"`
	PodUID         string               `json:"UID"`
	PodNamespace   string               `json:"namespace"`
	JobID          string               `json:"JID"`
	Containers     []v1.ContainerStatus `json:"containers"`
	InitContainers []v1.ContainerStatus `json:"initContainers"`
}

// RetrievedContainer is used in InterLink to rearrange data structure in a suitable way for the sidecar
type RetrievedContainer struct {
	Name       string         `json:"name"`
	ConfigMaps []v1.ConfigMap `json:"configMaps"`
	Secrets    []v1.Secret    `json:"secrets"`
	EmptyDirs  []string       `json:"emptyDirs"`
}

// InterLinkConfig holds the whole configuration
type InterLinkConfig struct {
	VKConfigPath        string   `yaml:"VKConfigPath"`
	VKTokenFile         string   `yaml:"VKTokenFile"`
	Interlinkurl        string   `yaml:"InterlinkURL"`
	Sidecarurl          string   `yaml:"SidecarURL"`
	Sbatchpath          string   `yaml:"SbatchPath"`
	Scancelpath         string   `yaml:"ScancelPath"`
	Squeuepath          string   `yaml:"SqueuePath"`
	Interlinkport       string   `yaml:"InterlinkPort"`
	Socket              string   `yaml:"Socket"`
	Sidecarport         string   `yaml:"SidecarPort"`
	Commandprefix       string   `yaml:"CommandPrefix"`
	ExportPodData       bool     `yaml:"ExportPodData"`
	DataRootFolder      string   `yaml:"DataRootFolder"`
	ServiceAccount      string   `yaml:"ServiceAccount"`
	Namespace           string   `yaml:"Namespace"`
	Tsocks              bool     `yaml:"Tsocks"`
	Tsockspath          string   `yaml:"TsocksPath"`
	Tsocksconfig        string   `yaml:"TsocksConfig"`
	Tsockslogin         string   `yaml:"TsocksLoginNode"`
	BashPath            string   `yaml:"BashPath"`
	VerboseLogging      bool     `yaml:"VerboseLogging"`
	ErrorsOnlyLogging   bool     `yaml:"ErrorsOnlyLogging"`
	PodIP               string   `yaml:"PodIP"`
	SingularityPrefix   string   `yaml:"SingularityPrefix"`
	DockerNetworkSubnet []string `yaml:"DockerNetworkSubnet"`
	set                 bool
}

// ContainerLogOpts is a struct in which it is possible to specify options to retrieve logs from the sidecar
type ContainerLogOpts struct {
	Tail         int       `json:"Tail"`
	LimitBytes   int       `json:"Bytes"`
	Timestamps   bool      `json:"Timestamps"`
	Follow       bool      `json:"Follow"`
	Previous     bool      `json:"Previous"`
	SinceSeconds int       `json:"SinceSeconds"`
	SinceTime    time.Time `json:"SinceTime"`
}

// LogStruct is needed to identify the job/container running on the sidecar to retrieve the logs from. Using ContainerLogOpts struct allows to specify more options on how to collect logs
type LogStruct struct {
	Namespace     string           `json:"Namespace"`
	PodUID        string           `json:"PodUID"`
	PodName       string           `json:"PodName"`
	ContainerName string           `json:"ContainerName"`
	Opts          ContainerLogOpts `json:"Opts"`
}

type SpanConfig struct {
	HTTPReturnCode int
	SetHTTPCode    bool
}

type SpanOption func(*SpanConfig)
