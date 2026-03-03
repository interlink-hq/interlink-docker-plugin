package docker

import (
	"fmt"
	"strings"
)

func cleanInnerScriptHeader(content string) string {
	lines := strings.Split(content, "\n")
	var cleanedLines []string
	skipHeader := true

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip the header section of the inner script
		if skipHeader {
			// Skip shebang
			if strings.HasPrefix(trimmed, "#!") {
				continue
			}
			// Skip set commands
			if trimmed == "set -e" || trimmed == "set -euo pipefail" {
				continue
			}
			// Skip PATH export that duplicates outer script
			if strings.HasPrefix(trimmed, "export PATH=$TMPDIR") ||
				strings.Contains(trimmed, "Ensure PATH includes tmpdir") {
				continue
			}
			// Skip empty lines at start
			if trimmed == "" {
				continue
			}
			// Once we hit actual content, stop skipping
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				skipHeader = false
			}
		}

		if !skipHeader {
			cleanedLines = append(cleanedLines, line)
		}
	}

	return strings.Join(cleanedLines, "\n")
}

func extractWGIfaceDefinition(content string) string {
	// Look for WG_IFACE definition in the outer script first
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "WG_IFACE=") || strings.HasPrefix(trimmed, "export WG_IFACE=") {
			return trimmed
		}
		// Also check in comments or before EOFSLIRP
		if strings.Contains(trimmed, "# Set WireGuard interface name") {
			// Next non-empty line should have the definition
			for i, l := range lines {
				if strings.TrimSpace(l) == trimmed {
					if i+1 < len(lines) {
						nextLine := strings.TrimSpace(lines[i+1])
						if strings.HasPrefix(nextLine, "WG_IFACE=") {
							return nextLine
						}
					}
				}
			}
		}
	}

	// If not found in outer script, try to find in EOFSLIRP
	innerScript, err := extractHeredoc(content, "EOFSLIRP")
	if err == nil {
		lines = strings.Split(innerScript, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "WG_IFACE=") {
				return trimmed
			}
		}
	}

	return ""
}

func removeWGIfaceDefinition(content string) string {
	lines := strings.Split(content, "\n")
	var cleanedLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Skip WG_IFACE definition lines
		if strings.HasPrefix(trimmed, "WG_IFACE=") ||
			strings.HasPrefix(trimmed, "export WG_IFACE=") ||
			trimmed == "# Get WireGuard interface name from parent" {
			continue
		}
		cleanedLines = append(cleanedLines, line)
	}

	return strings.Join(cleanedLines, "\n")
}

func extractWGConfigSection(content string) string {
	// Find the WireGuard config creation section
	wgStart := strings.Index(content, "# Create WireGuard config")
	if wgStart == -1 {
		wgStart = strings.Index(content, "cat <<'EOFWG'")
	}

	if wgStart == -1 {
		return ""
	}

	// Find where this section ends (before "Generate the execution script")
	wgEnd := strings.Index(content[wgStart:], "# Generate the execution script")
	if wgEnd == -1 {
		wgEnd = strings.Index(content[wgStart:], "cat <<'EOFSLIRP'")
	}

	if wgEnd == -1 {
		return ""
	}

	wgSection := strings.TrimSpace(content[wgStart : wgStart+wgEnd])

	// Ensure the config file is created in TMPDIR with explicit path
	wgSection = strings.Replace(wgSection,
		"> $WG_IFACE.conf",
		"> $TMPDIR/$WG_IFACE.conf",
		1)

	return wgSection
}

func extractDownloadSection(content string) string {
	// Find the start of downloads
	downloadStart := strings.Index(content, "echo \"=== Downloading binaries")
	if downloadStart == -1 {
		downloadStart = strings.Index(content, "# Download wstunnel")
	}

	if downloadStart == -1 {
		return ""
	}

	// Find the end of downloads (before the WireGuard config creation)
	downloadEnd := strings.Index(content[downloadStart:], "# Create WireGuard config")
	if downloadEnd == -1 {
		downloadEnd = strings.Index(content[downloadStart:], "cat <<'EOFWG'")
	}

	if downloadEnd == -1 {
		return ""
	}

	downloadSection := content[downloadStart : downloadStart+downloadEnd]

	// Remove slirp4netns download from the section
	lines := strings.Split(downloadSection, "\n")
	var cleanedLines []string
	skipSlirp := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Start skipping at slirp4netns download
		if strings.Contains(trimmed, "# Download slirp4netns") ||
			strings.Contains(trimmed, "echo \"Downloading slirp4netns...\"") {
			skipSlirp = true
			continue
		}

		// Stop skipping after the slirp4netns section
		if skipSlirp && (strings.HasPrefix(trimmed, "# ") && !strings.Contains(trimmed, "slirp")) {
			skipSlirp = false
		}

		// Skip lines related to slirp4netns
		if strings.Contains(line, "slirp4netns") {
			continue
		}

		if !skipSlirp {
			cleanedLines = append(cleanedLines, line)
		}
	}

	return strings.Join(cleanedLines, "\n")
}

func extractHeredoc(content, marker string) (string, error) {
	// Find the start of the heredoc
	startPattern := fmt.Sprintf("cat <<'%s'", marker)
	startIdx := strings.Index(content, startPattern)
	if startIdx == -1 {
		return "", fmt.Errorf("heredoc start marker not found")
	}

	// Find the line after the cat command (start of actual content)
	contentStart := strings.Index(content[startIdx:], "\n")
	if contentStart == -1 {
		return "", fmt.Errorf("invalid heredoc format")
	}
	contentStart += startIdx + 1

	// Find the end marker
	endMarker := "\n" + marker
	endIdx := strings.Index(content[contentStart:], endMarker)
	if endIdx == -1 {
		return "", fmt.Errorf("heredoc end marker not found")
	}

	// Extract the content between start and end markers
	return content[contentStart : contentStart+endIdx], nil
}

func removeHeredoc(content, marker string) string {
	// Find the start of the heredoc
	startPattern := fmt.Sprintf("cat <<'%s'", marker)
	startIdx := strings.Index(content, startPattern)
	if startIdx == -1 {
		return content // No heredoc found, return as-is
	}

	// Find the line after the cat command (start of actual content)
	contentStart := strings.Index(content[startIdx:], "\n")
	if contentStart == -1 {
		return content // Invalid heredoc format
	}
	contentStart += startIdx + 1

	// Find the end marker
	endMarker := "\n" + marker
	endIdx := strings.Index(content[contentStart:], endMarker)
	if endIdx == -1 {
		return content // Heredoc end marker not found
	}

	// Calculate the actual end position (after the end marker)
	heredocEnd := contentStart + endIdx + len(endMarker)

	// Skip trailing newline if present
	if heredocEnd < len(content) && content[heredocEnd] == '\n' {
		heredocEnd++
	}

	// Remove the heredoc block and return
	return content[:startIdx] + content[heredocEnd:]
}

func removeSlirp4netnsDownload(content string) string {
	// Remove the slirp4netns download section
	slirpDownloadStart := strings.Index(content, "# Download slirp4netns")
	if slirpDownloadStart == -1 {
		return content
	}

	// Find the end of this download block (next echo or next section)
	slirpDownloadEnd := strings.Index(content[slirpDownloadStart:], "# Check if iproute2")
	if slirpDownloadEnd == -1 {
		slirpDownloadEnd = strings.Index(content[slirpDownloadStart:], "\n\n")
	}

	if slirpDownloadEnd != -1 {
		return content[:slirpDownloadStart] + content[slirpDownloadStart+slirpDownloadEnd:]
	}

	return content
}

func removeUnshareWrapper(content string) string {
	// Remove the entire unshare mode detection and execution section
	unshareStart := strings.Index(content, "# Detect best unshare strategy")
	if unshareStart == -1 {
		unshareStart = strings.Index(content, "echo \"=== Starting network namespace ===\"")
	}

	if unshareStart == -1 {
		return content
	}

	// Remove everything from the unshare section to the end
	return content[:unshareStart]
}

func removeSlirp4netnsExecution(content string) string {
	// Remove slirp4netns execution lines
	lines := strings.Split(content, "\n")
	var cleanedLines []string

	skipUntilBlank := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip slirp4netns related lines
		if strings.Contains(trimmed, "./slirp4netns") ||
			strings.Contains(trimmed, "SLIRPPID") ||
			strings.Contains(trimmed, "slirp4netns_") ||
			strings.Contains(trimmed, "Bring the main job to foreground") ||
			strings.Contains(trimmed, "fg 1") {
			skipUntilBlank = true
			continue
		}

		// Skip comments about slirp4netns
		if strings.Contains(trimmed, "Create the tap0 device with slirp4netns") ||
			strings.Contains(trimmed, "Starting slirp4netns") ||
			strings.Contains(trimmed, "Wait a bit for slirp4netns") {
			continue
		}

		if skipUntilBlank && trimmed == "" {
			skipUntilBlank = false
			continue
		}

		if !skipUntilBlank {
			cleanedLines = append(cleanedLines, line)
		}
	}

	return strings.Join(cleanedLines, "\n")
}

func extractFinalDNSConfig(content string) (nameserver string, searchDomains string) {
	// Look for the final DNS configuration (the simple echo commands)
	// Pattern: echo "nameserver X.X.X.X" > /etc/resolv.conf
	//          echo "search ..." >> /etc/resolv.conf

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Find the nameserver line
		if strings.Contains(trimmed, "echo") && strings.Contains(trimmed, "nameserver") && strings.Contains(trimmed, "> /etc/resolv.conf") {
			// Extract nameserver IP
			// Pattern: echo "nameserver 10.43.0.10" > /etc/resolv.conf
			start := strings.Index(trimmed, "nameserver")
			if start != -1 {
				afterNameserver := trimmed[start+len("nameserver"):]
				fields := strings.Fields(afterNameserver)
				if len(fields) > 0 {
					nameserver = strings.Trim(fields[0], `"'`)
				}
			}

			// Look for the next line with search domains
			if i+1 < len(lines) {
				nextLine := strings.TrimSpace(lines[i+1])
				if strings.Contains(nextLine, "echo") && strings.Contains(nextLine, "search") && strings.Contains(nextLine, ">> /etc/resolv.conf") {
					// Extract search domains
					// Pattern: echo "search mlaas.svc.cluster.local svc.cluster.local cluster.local" >> /etc/resolv.conf
					start := strings.Index(nextLine, `"search`)
					end := strings.LastIndex(nextLine, `"`)
					if start != -1 && end != -1 && end > start {
						searchContent := nextLine[start+1 : end]
						// Remove the "search " prefix
						searchDomains = strings.TrimPrefix(searchContent, "search ")
					}
				}
			}
			break
		}
	}

	return nameserver, searchDomains
}

func removeFinalDNSConfig(content string) string {
	// Remove the final DNS configuration lines
	lines := strings.Split(content, "\n")
	var cleanedLines []string
	skipNext := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if skipNext {
			// Skip this line (the search domains line)
			skipNext = false
			continue
		}

		// Check if this is the nameserver echo line
		if strings.Contains(trimmed, "echo") &&
			strings.Contains(trimmed, "nameserver") &&
			strings.Contains(trimmed, "> /etc/resolv.conf") &&
			!strings.Contains(trimmed, ">>") {
			// Skip this line and set flag to skip next line
			skipNext = true
			continue
		}

		cleanedLines = append(cleanedLines, line)
	}

	return strings.Join(cleanedLines, "\n")
}
