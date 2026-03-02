package common

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"k8s.io/client-go/kubernetes"

	"github.com/containerd/containerd/log"
	"gopkg.in/yaml.v2"

	trace "go.opentelemetry.io/otel/trace"
)

var InterLinkConfigInst DockerConfig
var Clientset *kubernetes.Clientset

// TODO: implement factory design

// NewInterLinkConfig returns a variable of type InterLinkConfig, used in many other functions and the first encountered error.
func NewInterLinkConfig() (DockerConfig, error) {
	if !InterLinkConfigInst.set {
		var path string
		verbose := flag.Bool("verbose", false, "Enable or disable Debug level logging")
		errorsOnly := flag.Bool("errorsonly", false, "Prints only errors if enabled")
		InterLinkConfigPath := flag.String("interlinkconfigpath", "", "Path to InterLink config")
		flag.Parse()

		if *verbose {
			InterLinkConfigInst.VerboseLogging = true
			InterLinkConfigInst.ErrorsOnlyLogging = false
		} else if *errorsOnly {
			InterLinkConfigInst.VerboseLogging = false
			InterLinkConfigInst.ErrorsOnlyLogging = true
		}

		if *InterLinkConfigPath != "" {
			path = *InterLinkConfigPath
		} else if os.Getenv("INTERLINKCONFIGPATH") != "" {
			path = os.Getenv("INTERLINKCONFIGPATH")
		} else {
			path = "/etc/interlink/InterLinkConfig.yaml"
		}

		if _, err := os.Stat(path); err != nil {
			log.G(context.Background()).Error("File " + path + " doesn't exist. You can set a custom path by exporting INTERLINKCONFIGPATH. Exiting...")
			return DockerConfig{}, err
		}

		log.G(context.Background()).Info("\u2705 Loading InterLink config from " + path)
		yfile, err := os.ReadFile(path)
		if err != nil {
			log.G(context.Background()).Error("\u274C Error opening config file, exiting...")
			return DockerConfig{}, err
		}
		yaml.Unmarshal(yfile, &InterLinkConfigInst)

		if os.Getenv("TSOCKS") != "" {
			if os.Getenv("TSOCKS") != "true" && os.Getenv("TSOCKS") != "false" {
				fmt.Println("export TSOCKS as true or false")
				return DockerConfig{}, err
			}
			if os.Getenv("TSOCKS") == "true" {
				InterLinkConfigInst.Tsocks = true
			} else {
				InterLinkConfigInst.Tsocks = false
			}
		}

		if os.Getenv("TSOCKSPATH") != "" {
			path = os.Getenv("TSOCKSPATH")
			if _, err := os.Stat(path); err != nil {
				log.G(context.Background()).Error("File " + path + " doesn't exist. You can set a custom path by exporting TSOCKSPATH. Exiting...")
				return DockerConfig{}, err
			}

			InterLinkConfigInst.Tsockspath = path
		}

		InterLinkConfigInst.set = true
	}
	return InterLinkConfigInst, nil
}

func WithHTTPReturnCode(code int) SpanOption {
	return func(cfg *SpanConfig) {
		cfg.HTTPReturnCode = code
		cfg.SetHTTPCode = true
	}
}

func SetDurationSpan(startTime int64, span trace.Span, opts ...SpanOption) {
	endTime := time.Now().UnixMicro()
	config := &SpanConfig{}

	for _, opt := range opts {
		opt(config)
	}

	duration := endTime - startTime
	span.SetAttributes(attribute.Int64("end.timestamp", endTime),
		attribute.Int64("duration", duration))

	if config.SetHTTPCode {
		span.SetAttributes(attribute.Int("exit.code", config.HTTPReturnCode))
	}
}
