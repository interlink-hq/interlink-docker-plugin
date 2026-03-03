package docker

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	OSexec "os/exec"

	"github.com/containerd/containerd/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
)

func handleError(span trace.Span, err error, statusCode int, start int64) {
	span.SetAttributes(attribute.String("error", err.Error()))
	commonIL.SetDurationSpan(start, span, commonIL.WithHTTPReturnCode(statusCode))
	span.End()
}

// GetLogsHandler performs a Docker logs command and returns its manipulated output
func (h *SidecarHandler) GetLogsHandler(w http.ResponseWriter, r *http.Request) {
	log.G(h.Ctx).Info("\u23F3 [LOGS CALL]: received get logs call")

	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	_, span := tracer.Start(h.Ctx, "GetLogs", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))

	var req commonIL.LogStruct
	statusCode := http.StatusOK
	currentTime := time.Now()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
		log.G(h.Ctx).Error(err)
		handleError(span, err, statusCode, start)
		return
	}

	err = json.Unmarshal(bodyBytes, &req)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
		log.G(h.Ctx).Error(err)
		handleError(span, err, statusCode, start)
		return
	}

	podUID := string(req.PodUID)
	podNamespace := string(req.Namespace)

	containerName := podNamespace + "-" + podUID + "-" + req.ContainerName

	cmd := OSexec.Command("docker", "exec", podUID+"_dind", "docker", "ps", "-a", "--format", "{{.Names}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.G(h.Ctx).Error(err)
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		handleError(span, err, statusCode, start)
		return
	}

	lines := strings.Split(string(output), "\n")
	found := false
	for _, line := range lines {
		if line == containerName {
			found = true
			break
		}
	}
	if !found {
		w.WriteHeader(statusCode)
		w.Write([]byte("No logs available for container " + containerName + ". Container not found."))
		return
	}

	//var cmd *OSexec.Cmd
	if req.Opts.Timestamps {
		cmd = OSexec.Command("docker", "exec", podUID+"_dind", "docker", "logs", "-t", containerName)
	} else {
		cmd = OSexec.Command("docker", "exec", podUID+"_dind", "docker", "logs", containerName)
	}

	output, err = cmd.CombinedOutput()

	if err != nil {
		log.G(h.Ctx).Error(err)
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		handleError(span, err, statusCode, start)
		return
	}

	var returnedLogs string

	if req.Opts.Tail != 0 {
		var lastLines []string

		splittedLines := strings.Split(string(output), "\n")

		if req.Opts.Tail > len(splittedLines) {
			lastLines = splittedLines
		} else {
			lastLines = splittedLines[len(splittedLines)-req.Opts.Tail-1:]
		}

		for _, line := range lastLines {
			returnedLogs += line + "\n"
		}
	} else if req.Opts.LimitBytes != 0 {
		var lastBytes []byte
		if req.Opts.LimitBytes > len(output) {
			lastBytes = output
		} else {
			lastBytes = output[len(output)-req.Opts.LimitBytes-1:]
		}

		returnedLogs = string(lastBytes)
	} else {
		returnedLogs = string(output)
	}

	if req.Opts.Timestamps && (req.Opts.SinceSeconds != 0 || !req.Opts.SinceTime.IsZero()) {
		temp := returnedLogs
		returnedLogs = ""
		splittedLogs := strings.Split(temp, "\n")
		timestampFormat := "2006-01-02T15:04:05.999999999Z"

		for _, Log := range splittedLogs {
			part := strings.SplitN(Log, " ", 2)
			timestampString := part[0]
			timestamp, err := time.Parse(timestampFormat, timestampString)
			if err != nil {
				continue
			}
			if req.Opts.SinceSeconds != 0 {
				if currentTime.Sub(timestamp).Seconds() > float64(req.Opts.SinceSeconds) {
					returnedLogs += Log + "\n"
				}
			} else {
				if timestamp.Sub(req.Opts.SinceTime).Seconds() >= 0 {
					returnedLogs += Log + "\n"
				}
			}
		}
	}

	if statusCode != http.StatusOK {
		w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
	} else {
		w.WriteHeader(statusCode)
		w.Write([]byte(returnedLogs))
	}

	if err != nil {
		span.SetAttributes(attribute.String("error", err.Error()))
	}
	commonIL.SetDurationSpan(start, span, commonIL.WithHTTPReturnCode(statusCode))
	span.End()
}
