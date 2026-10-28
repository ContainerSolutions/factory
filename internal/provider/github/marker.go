package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/provider"
)

// The projection marker (github-provider.md §Observe and projection): one hidden comment
// per (work item, object), authored by the author App:
//
//	<!-- factory:wi v=1 id=wi_01JA… state=release:drafting,train:compliance-framework%2Fworkflows%23123 runs=release-docs:1 links=github-public:compliance-framework%2Fdocs%2357:pr:primary labels=factory%3Atracked -->
//	🏭 Tracked by CCF Factory — [view](https://<public-url>/workitems/wi_01JA…)
//
// Fields are space-separated key=value pairs after the version; every key and value is
// percent-encoded (only A-Z a-z 0-9 . _ ~ - stay as they are), so a value can hold no space,
// comma, colon, ">" or "-->". state is k:v pairs, a string value encoded as is and any other
// value (number, bool, map, list) as "b64:" + base64url(JSON); runs are the counters, k:n;
// links are provider:ref[:kind[:primary]]; labels are the factory-owned labels this work item
// put on the object. Unknown keys are ignored, so v=1 can grow optional fields; a marker of
// another version is not read.
const (
	markerPrefix  = "<!-- factory:wi "
	markerEnd     = " -->"
	markerVersion = "1"
	// MarkerPhrase is the marker's visible text, for the people reading the object.
	MarkerPhrase = "Tracked by CCF Factory"
	// MaxMarkerBytes caps a marker comment (GitHub's limit is 65 536 characters). A projection
	// whose marker would be larger is written without its state, which then lives only in the
	// dispatcher, and a warning is logged.
	MaxMarkerBytes = 60 << 10
	b64Prefix      = "b64:"
)

// formatMarker renders p's marker comment. uiBase, when set, is the factory's public URL the
// visible line links to. withState false leaves the state out (an oversized marker).
func formatMarker(p provider.Projection, uiBase string, withState bool) (string, error) {
	if p.WorkItemID == "" {
		return "", fmt.Errorf("projection has no work item ID: %w", provider.ErrInvalid)
	}
	var b strings.Builder
	b.WriteString(markerPrefix)
	b.WriteString("v=" + markerVersion)
	b.WriteString(" id=" + escape(p.WorkItemID))
	if withState && len(p.State) > 0 {
		pairs := make([]string, 0, len(p.State))
		for _, k := range slices.Sorted(maps.Keys(p.State)) {
			v, err := stateValue(p.State[k])
			if err != nil {
				return "", fmt.Errorf("state %q: %w", k, err)
			}
			pairs = append(pairs, escape(k)+":"+v)
		}
		b.WriteString(" state=" + strings.Join(pairs, ","))
	}
	if len(p.Counters) > 0 {
		pairs := make([]string, 0, len(p.Counters))
		for _, k := range slices.Sorted(maps.Keys(p.Counters)) {
			pairs = append(pairs, escape(k)+":"+strconv.Itoa(p.Counters[k]))
		}
		b.WriteString(" runs=" + strings.Join(pairs, ","))
	}
	if len(p.Links) > 0 {
		links := make([]string, 0, len(p.Links))
		for _, l := range p.Links {
			s := escape(l.Provider) + ":" + escape(l.Ref)
			switch {
			case l.Primary:
				s += ":" + escape(l.Kind) + ":primary"
			case l.Kind != "":
				s += ":" + escape(l.Kind)
			}
			links = append(links, s)
		}
		b.WriteString(" links=" + strings.Join(links, ","))
	}
	if labels := ownedLabels(p.Labels); len(labels) > 0 {
		enc := make([]string, len(labels))
		for i, l := range labels {
			enc[i] = escape(l)
		}
		b.WriteString(" labels=" + strings.Join(enc, ","))
	}
	b.WriteString(markerEnd)
	b.WriteString("\n🏭 " + MarkerPhrase)
	if uiBase != "" {
		b.WriteString(" — [view](" + strings.TrimRight(uiBase, "/") + "/workitems/" + url.PathEscape(p.WorkItemID) + ")")
	}
	return b.String(), nil
}

// stateValue encodes one state value: a string as itself, anything else as b64:JSON.
func stateValue(v any) (string, error) {
	if s, ok := v.(string); ok {
		return escape(s), nil
	}
	j, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("not JSON: %w: %w", err, provider.ErrInvalid)
	}
	return b64Prefix + base64.RawURLEncoding.EncodeToString(j), nil
}

// ownedLabels is labels sorted, without duplicates or empty names.
func ownedLabels(labels []string) []string {
	out := slices.DeleteFunc(slices.Clone(labels), func(l string) bool { return l == "" })
	slices.Sort(out)
	return slices.Compact(out)
}

// isMarker reports whether a comment body is a projection marker of any version.
func isMarker(body string) bool { return strings.HasPrefix(body, markerPrefix) }

// parseMarker reads a v=1 marker back. ok is false for a body that isn't a marker, a marker of
// another version, or one that doesn't parse; err says why for the last two.
func parseMarker(body string) (p provider.Projection, ok bool, err error) {
	if !isMarker(body) {
		return provider.Projection{}, false, nil
	}
	head, _, found := strings.Cut(body[len(markerPrefix):], markerEnd)
	if !found {
		return provider.Projection{}, false, fmt.Errorf("marker: no end")
	}
	fields := strings.Fields(head)
	if len(fields) == 0 || fields[0] != "v="+markerVersion {
		return provider.Projection{}, false, fmt.Errorf("marker: not version %s", markerVersion)
	}
	for _, f := range fields[1:] {
		k, v, _ := strings.Cut(f, "=")
		if v == "" {
			continue
		}
		var err error
		switch k {
		case "id":
			p.WorkItemID, err = url.PathUnescape(v)
		case "state":
			p.State, err = parseState(v)
		case "runs":
			p.Counters, err = parseRuns(v)
		case "links":
			p.Links, err = parseLinks(v)
		case "labels":
			p.Labels, err = parseList(v)
		}
		if err != nil {
			return provider.Projection{}, false, fmt.Errorf("marker: %s: %w", k, err)
		}
	}
	if p.WorkItemID == "" {
		return provider.Projection{}, false, fmt.Errorf("marker: no id")
	}
	return p, true, nil
}

func parseState(v string) (map[string]any, error) {
	out := map[string]any{}
	for pair := range strings.SplitSeq(v, ",") {
		ek, ev, ok := strings.Cut(pair, ":")
		if !ok {
			return nil, fmt.Errorf("pair %q has no value", pair)
		}
		k, err := url.PathUnescape(ek)
		if err != nil {
			return nil, err
		}
		if enc, ok := strings.CutPrefix(ev, b64Prefix); ok {
			j, err := base64.RawURLEncoding.DecodeString(enc)
			if err != nil {
				return nil, fmt.Errorf("value of %q: %w", k, err)
			}
			var val any
			if err := json.Unmarshal(j, &val); err != nil {
				return nil, fmt.Errorf("value of %q: %w", k, err)
			}
			out[k] = val
			continue
		}
		if out[k], err = url.PathUnescape(ev); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func parseRuns(v string) (map[string]int, error) {
	out := map[string]int{}
	for pair := range strings.SplitSeq(v, ",") {
		ek, ev, ok := strings.Cut(pair, ":")
		if !ok {
			return nil, fmt.Errorf("pair %q has no count", pair)
		}
		k, err := url.PathUnescape(ek)
		if err != nil {
			return nil, err
		}
		if out[k], err = strconv.Atoi(ev); err != nil {
			return nil, fmt.Errorf("count of %q: %w", k, err)
		}
	}
	return out, nil
}

func parseLinks(v string) ([]v1alpha1.Link, error) {
	var out []v1alpha1.Link
	for item := range strings.SplitSeq(v, ",") {
		parts := strings.Split(item, ":")
		if len(parts) < 2 || len(parts) > 4 || (len(parts) == 4 && parts[3] != "primary") {
			return nil, fmt.Errorf("link %q", item)
		}
		dec := make([]string, 3)
		for i := range min(len(parts), 3) {
			s, err := url.PathUnescape(parts[i])
			if err != nil {
				return nil, err
			}
			dec[i] = s
		}
		out = append(out, v1alpha1.Link{Provider: dec[0], Ref: dec[1], Kind: dec[2], Primary: len(parts) == 4})
	}
	return out, nil
}

func parseList(v string) ([]string, error) {
	var out []string
	for item := range strings.SplitSeq(v, ",") {
		s, err := url.PathUnescape(item)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// escape percent-encodes every byte but A-Z a-z 0-9 . _ ~ -.
func escape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '_', c == '~', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}
