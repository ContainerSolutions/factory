package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// The workspace drivers (P1 §1).
const (
	DriverTart = "tart"
	DriverKata = "kata"
)

// Workspace is the VM workspace setup (P1 §7): the driver, the per-workspace size, the gateway
// port and the warm base's images (used from P1 phase 2b).
type Workspace struct {
	Driver      string        `json:"driver,omitzero"` // tart | kata; default tart on darwin, kata on linux
	Size        WorkspaceSize `json:"size,omitzero"`
	GatewayPort int           `json:"gatewayPort,omitzero"` // default 3128
	Warm        WarmImages    `json:"warm,omitzero"`
}

// WorkspaceSize is one workspace's VM size.
type WorkspaceSize struct {
	CPUs   int    `json:"cpus,omitzero"`   // default 6
	Memory string `json:"memory,omitzero"` // Gi suffix, default 8Gi
	Disk   string `json:"disk,omitzero"`   // Gi suffix, default 60Gi
}

// WarmImages are the images the warm base's Docker cache holds; KindImages are also loaded
// into kind. A kind image is name:tag, or name:tag@sha256:... to pin it: the warm base pulls it
// by digest and saves it tagged name:tag, the reference kind's nodes find it by. A digest
// without a tag (name@sha256:...) is refused, since kind's nodes would have no name for it.
type WarmImages struct {
	Images     []string `json:"images,omitzero"`
	KindImages []string `json:"kindImages,omitzero"`
}

// WithDefaults fills the unset fields; goos picks the driver.
func (w Workspace) WithDefaults(goos string) Workspace {
	if w.Driver == "" {
		w.Driver = DriverKata
		if goos == "darwin" {
			w.Driver = DriverTart
		}
	}
	if w.Size.CPUs == 0 {
		w.Size.CPUs = 6
	}
	if w.Size.Memory == "" {
		w.Size.Memory = "8Gi"
	}
	if w.Size.Disk == "" {
		w.Size.Disk = "60Gi"
	}
	if w.GatewayPort == 0 {
		w.GatewayPort = 3128
	}
	return w
}

// MemoryGiB is size.memory in GiB; 0 when it isn't valid.
func (s WorkspaceSize) MemoryGiB() int { return gib(s.Memory) }

// DiskGiB is size.disk in GiB; 0 when it isn't valid.
func (s WorkspaceSize) DiskGiB() int { return gib(s.Disk) }

var gibRE = regexp.MustCompile(`^[1-9][0-9]{0,5}Gi$`)

func gib(v string) int {
	if !gibRE.MatchString(v) {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSuffix(v, "Gi"))
	return n
}

// maxImages and maxImageLen bound the warm image lists.
const (
	maxImages   = 100
	maxImageLen = 200
)

// validate checks a workspace with its defaults filled.
func (w Workspace) validate() []string {
	var probs []string
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf("workspace."+format, args...)) }
	if w.Driver != DriverTart && w.Driver != DriverKata {
		add("driver: %q is not one of %s, %s", w.Driver, DriverTart, DriverKata)
	}
	if w.Size.CPUs < 1 || w.Size.CPUs > 16 {
		add("size.cpus: %d is not from 1 to 16", w.Size.CPUs)
	}
	for _, s := range []struct {
		key, v   string
		min, max int
	}{{"memory", w.Size.Memory, 2, 64}, {"disk", w.Size.Disk, 20, 500}} {
		if n := gib(s.v); n < s.min || n > s.max {
			add("size.%s: %q is not from %dGi to %dGi (a whole number of Gi)", s.key, s.v, s.min, s.max)
		}
	}
	if w.GatewayPort < 1024 || w.GatewayPort > 65535 {
		add("gatewayPort: %d is not from 1024 to 65535", w.GatewayPort)
	}
	for _, l := range []struct {
		key    string
		images []string
	}{{"images", w.Warm.Images}, {"kindImages", w.Warm.KindImages}} {
		if len(l.images) > maxImages {
			add("warm.%s: %d images, at most %d", l.key, len(l.images), maxImages)
		}
		for i, im := range l.images {
			// host init passes each to docker pull as an argument: a leading - would be a flag.
			if im == "" || len(im) > maxImageLen || strings.ContainsFunc(im, unicode.IsSpace) || strings.HasPrefix(im, "-") {
				add("warm.%s[%d]: %q must be non-empty, at most %d bytes, without whitespace or a leading -", l.key, i, im, maxImageLen)
			}
			if _, ok := KindImageTag(im); l.key == "kindImages" && !ok {
				add("warm.%s[%d]: %q has a digest but no tag: use name:tag or name:tag@sha256:...", l.key, i, im)
			}
		}
	}
	return probs
}

// KindImageTag is the reference the warm base saves a kind image as: ref without its
// @digest, if any. ok is false for a digest without a tag (name@sha256:...).
func KindImageTag(ref string) (tagged string, ok bool) {
	name, _, digest := strings.Cut(ref, "@")
	if !digest {
		return ref, true
	}
	// the tag is after the last path component's colon; a colon before a / is a registry port.
	if !strings.Contains(name[strings.LastIndex(name, "/")+1:], ":") {
		return "", false
	}
	return name, true
}
