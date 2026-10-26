package host

// The pinned releases host init installs. Bump a pin by downloading the release on a trusted
// machine and recording the sha256 of what was downloaded (and, for Softnet, of the binary in
// it); never copy a checksum from the release page alone.
const (
	// TartVersion is the pinned Tart: https://github.com/cirruslabs/tart/releases/tag/2.41.1.
	TartVersion = "2.41.1"
	TartURL     = "https://github.com/cirruslabs/tart/releases/download/" + TartVersion + "/tart.tar.gz"
	// TartSHA256 is the sha256 of tart.tar.gz: `shasum -a 256` of the release asset, recorded
	// when the pin was set (P1 phase 2a).
	TartSHA256 = "e680a734febccfce7d4e910c1f7f13fc3b00f19a181c945e2d746de3214c32c2"
	// TartTeamID is Cirrus Labs' Developer ID team: `codesign -dv` of the notarized tart.app
	// reports it as TeamIdentifier.
	TartTeamID = "9M2P8L4D89"

	// SoftnetVersion is the pinned Softnet: https://github.com/cirruslabs/softnet/releases/tag/0.24.1.
	SoftnetVersion = "0.24.1"
	SoftnetURL     = "https://github.com/cirruslabs/softnet/releases/download/" + SoftnetVersion + "/softnet.tar.gz"
	// SoftnetSHA256 is the sha256 of softnet.tar.gz: `shasum -a 256` of the release asset,
	// recorded when the pin was set (P1 phase 2a).
	SoftnetSHA256 = "8ff8ff099c2b5ac1b1128e7999c4a730ac85f486c8a4cb0239eb98432859a0f7"
	// SoftnetBinarySHA256 is the sha256 of the softnet binary in that tarball (it matches the
	// 0.24.1 binary installed by hand in P1 phase 0). Softnet isn't code-signed, so the two
	// checksums are its only verification.
	SoftnetBinarySHA256 = "e5e35d61a6dd7e35a3134732e766c897b7cb6f097080ff00e1e75adeab0471c7"

	// BaseImage is the image the base VM is cloned from: cirruslabs' Ubuntu 24.04, pinned by
	// its manifest digest (the one P1 phase 0 ran). Tart keeps it in its OCI cache, so a
	// rebuild downloads nothing. The tools the base adds are pinned in provision/base.sh.
	BaseImage = "ghcr.io/cirruslabs/ubuntu@sha256:51b493a05e4bc6f21c5c5052cffc1b26b5cea1d76847f5db10b97da90c16e58c"
)

// The kata driver's pins (workspace-kata.md#steps-kata-driver), the releases P1 phase 9's Q8 dry
// run installed on the Linux workstation.
const (
	// NerdctlVersion is the pinned nerdctl-full, containerd, nerdctl, BuildKit, runc and the CNI
	// plugins in one release: https://github.com/containerd/nerdctl/releases/tag/v2.4.1.
	NerdctlVersion = "2.4.1"
	NerdctlURL     = "https://github.com/containerd/nerdctl/releases/download/v" + NerdctlVersion + "/nerdctl-full-" + NerdctlVersion + "-linux-amd64.tar.gz"
	// NerdctlSHA256 is the sha256 of nerdctl-full-2.4.1-linux-amd64.tar.gz: the asset's digest
	// in GitHub's releases API, which matches the release's SHA256SUMS.
	NerdctlSHA256 = "ff018f11ac2f1354bf7b3aaaca6e0b52d81139c746a89134744d0b7be3a0e62f"
	// NerdctlContainerdVersion is the containerd in that release (its notes: "Update containerd
	// (2.4.1)"); containerd --version reports it with a leading v.
	NerdctlContainerdVersion = "2.4.1"

	// KataVersion is the pinned Kata Containers static release (runtime-rs, Cloud Hypervisor,
	// the guest kernel and image): https://github.com/kata-containers/kata-containers/releases/tag/4.2.0,
	// the version phase 0 ran; 4.3.x was under two weeks old when pinned.
	KataVersion = "4.2.0"
	KataURL     = "https://github.com/kata-containers/kata-containers/releases/download/" + KataVersion + "/kata-static-" + KataVersion + "-amd64.tar.zst"
	// KataSHA256 is the sha256 of kata-static-4.2.0-amd64.tar.zst (about 950 MiB): the asset's
	// digest in GitHub's releases API.
	KataSHA256 = "b828904fa3f1e49ddd7dc799c72cb1503cd1e772d354c3987c8d4189b2a623a8"
)

// Pins are the releases a host init installs; tests replace them with fakes.
type Pins struct {
	TartVersion, TartURL, TartSHA256, TartTeamID   string
	SoftnetURL, SoftnetSHA256, SoftnetBinarySHA256 string
	BaseImage                                      string
	// The kata driver's.
	NerdctlVersion, NerdctlURL, NerdctlSHA256, NerdctlContainerdVersion string
	KataVersion, KataURL, KataSHA256                                    string
}

// DefaultPins are the pinned releases above.
func DefaultPins() Pins {
	return Pins{
		TartVersion: TartVersion, TartURL: TartURL, TartSHA256: TartSHA256, TartTeamID: TartTeamID,
		SoftnetURL: SoftnetURL, SoftnetSHA256: SoftnetSHA256, SoftnetBinarySHA256: SoftnetBinarySHA256,
		BaseImage:      BaseImage,
		NerdctlVersion: NerdctlVersion, NerdctlURL: NerdctlURL, NerdctlSHA256: NerdctlSHA256, NerdctlContainerdVersion: NerdctlContainerdVersion,
		KataVersion: KataVersion, KataURL: KataURL, KataSHA256: KataSHA256,
	}
}
