package docker

import "testing"

func names(structs []DockerRunStruct) []string {
	out := make([]string, 0, len(structs))
	for _, s := range structs {
		out = append(out, s.Name)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSelectNetworkOverlay(t *testing.T) {
	overlay := DockerRunStruct{Name: "ns-uid-network-overlay"}
	appC := DockerRunStruct{Name: "ns-uid-app"}
	sidecar := DockerRunStruct{Name: "ns-uid-sidecar"}

	tests := []struct {
		name         string
		containers   []DockerRunStruct
		overlayName  string
		wantOK       bool
		wantOverlay  string
		wantWorkload []string
	}{
		{
			name:         "overlay first, as prepended",
			containers:   []DockerRunStruct{overlay, appC, sidecar},
			overlayName:  overlay.Name,
			wantOK:       true,
			wantOverlay:  overlay.Name,
			wantWorkload: []string{appC.Name, sidecar.Name},
		},
		{
			name:         "overlay not at index 0 is still found, order preserved",
			containers:   []DockerRunStruct{appC, overlay, sidecar},
			overlayName:  overlay.Name,
			wantOK:       true,
			wantOverlay:  overlay.Name,
			wantWorkload: []string{appC.Name, sidecar.Name},
		},
		{
			name:         "overlay is the only container",
			containers:   []DockerRunStruct{overlay},
			overlayName:  overlay.Name,
			wantOK:       true,
			wantOverlay:  overlay.Name,
			wantWorkload: []string{},
		},
		{
			// The regression: a mesh annotation was present but no overlay was built.
			// containers[0] used to be started as the overlay, and every remaining
			// container then waited forever on a sentinel nothing would write.
			name:         "no overlay built: no workload container is mistaken for it",
			containers:   []DockerRunStruct{appC, sidecar},
			overlayName:  "",
			wantOK:       false,
			wantWorkload: []string{appC.Name, sidecar.Name},
		},
		{
			name:         "overlay name set but absent from the list",
			containers:   []DockerRunStruct{appC, sidecar},
			overlayName:  overlay.Name,
			wantOK:       false,
			wantWorkload: []string{appC.Name, sidecar.Name},
		},
		{
			// Used to panic on containers[0].
			name:         "empty container list",
			containers:   nil,
			overlayName:  overlay.Name,
			wantOK:       false,
			wantWorkload: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotOverlay, gotWorkload, gotOK := selectNetworkOverlay(tc.containers, tc.overlayName)

			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotOverlay.Name != tc.wantOverlay {
				t.Errorf("overlay = %q, want %q", gotOverlay.Name, tc.wantOverlay)
			}
			if got := names(gotWorkload); !equal(got, tc.wantWorkload) {
				t.Errorf("workload = %v, want %v", got, tc.wantWorkload)
			}
		})
	}
}

// The original list must not be modified: it is still used by the non-mesh
// startup path.
func TestSelectNetworkOverlayDoesNotMutateInput(t *testing.T) {
	containers := []DockerRunStruct{
		{Name: "ns-uid-network-overlay"},
		{Name: "ns-uid-app"},
		{Name: "ns-uid-sidecar"},
	}
	before := names(containers)

	if _, _, ok := selectNetworkOverlay(containers, "ns-uid-network-overlay"); !ok {
		t.Fatal("expected the overlay to be found")
	}

	if after := names(containers); !equal(before, after) {
		t.Errorf("input list was mutated: %v -> %v", before, after)
	}
}
