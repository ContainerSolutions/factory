package policy

import (
	"net/url"
	"slices"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/runbundle"
)

func TestHostClasses(t *testing.T) {
	tests := []struct {
		host        string
		class       Class
		intercepted bool
		strips      bool
		cookies     CookieRule
	}{
		{"api.anthropic.com", Anthropic, true, true, CookiesInfraOnly},
		{"claude.ai", Anthropic, true, true, CookiesInfraOnly},
		{"console.anthropic.com", Anthropic, true, true, CookiesInfraOnly},
		{"platform.claude.com", Anthropic, true, true, CookiesInfraOnly},
		{"Claude.AI.", Anthropic, true, true, CookiesInfraOnly},
		{"chatgpt.com", ChatGPT, true, true, CookiesInfraOnly},
		{"auth.openai.com", AuthOpenAI, false, false, CookiesUntouched},
		{"github.com", GitHub, true, true, CookiesStripped},
		{"GITHUB.COM.", GitHub, true, true, CookiesStripped},
		{"api.github.com", GitHubAPI, true, true, CookiesStripped},
		{"dispatcher.factory.internal", Dispatcher, false, true, CookiesStripped},
		// Exact names only: a name under an intercepted host, or above it, is ordinary.
		{"downloads.claude.ai", Ordinary, false, false, CookiesUntouched},
		{"api.openai.com", Ordinary, false, false, CookiesUntouched}, // departure D1
		{"anthropic.com", Ordinary, false, false, CookiesUntouched},
		{"gist.github.com", Ordinary, false, false, CookiesUntouched},
		{"objects.githubusercontent.com", Ordinary, false, false, CookiesUntouched},
		{"github.com..", Ordinary, false, false, CookiesUntouched},
		{"conſole.anthropic.com", Ordinary, false, false, CookiesUntouched}, // no Unicode folding (a long s)
		{"diſpatcher.factory.internal", Ordinary, false, false, CookiesUntouched},
		{"140.82.112.3", Ordinary, false, false, CookiesUntouched},
		{"", Ordinary, false, false, CookiesUntouched},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			c := ClassOf(tt.host)
			if c != tt.class || c.Intercepted() != tt.intercepted || c.StripsCarriers() != tt.strips || c.Cookies() != tt.cookies {
				t.Errorf("ClassOf(%q) = %s (intercepted %v, strips %v, cookies %d), want %s (%v, %v, %d)",
					tt.host, c, c.Intercepted(), c.StripsCarriers(), c.Cookies(), tt.class, tt.intercepted, tt.strips, tt.cookies)
			}
		})
	}
	if s := Class(200).String(); s != "unknown" {
		t.Errorf("Class(200).String() = %q", s)
	}
}

func TestHostLists(t *testing.T) {
	want := []string{"api.anthropic.com", "api.github.com", "chatgpt.com", "claude.ai", "console.anthropic.com", "github.com", "platform.claude.com"}
	if got := InterceptedHosts(); !slices.Equal(got, want) {
		t.Errorf("InterceptedHosts() = %v, want %v", got, want)
	}
	for _, h := range InterceptedHosts() {
		if !ClassOf(h).Intercepted() {
			t.Errorf("%s is listed but not intercepted", h)
		}
	}
	a := AnthropicHosts()
	a[0] = "changed"
	if AnthropicHosts()[0] == "changed" {
		t.Error("AnthropicHosts returned the list itself, not a copy")
	}
	if got := CarrierHeaders(); !slices.Equal(got, []string{"Authorization", "Proxy-Authorization", "X-Api-Key"}) {
		t.Errorf("CarrierHeaders() = %v", got)
	}
	u, err := url.Parse(runbundle.DefaultDispatcherURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != DispatcherHost || u.Port() != "" || u.Scheme != "http" {
		t.Errorf("runbundle.DefaultDispatcherURL = %q, want http://%s on the default port", runbundle.DefaultDispatcherURL, DispatcherHost)
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in       string
		connect  bool   // ParseConnectTarget accepts it
		plain    bool   // ParseHostPort(in, 80) accepts it
		wantHost string // when accepted
		wantPort uint16 // ParseConnectTarget's; ParseHostPort's is 80 when the input has none
		ip       bool
	}{
		{"github.com:443", true, true, "github.com", 443, false},
		{"GitHub.COM.:443", true, true, "github.com", 443, false},
		{"github.com", false, true, "github.com", 0, false},
		{"under_score.example.com:8080", true, true, "under_score.example.com", 8080, false},
		{"1.2.3.4:443", true, true, "1.2.3.4", 443, true},
		{"1.2.3.4.:443", true, true, "1.2.3.4", 443, true}, // a literal, not a name
		{"[2606:4700::1]:443", true, true, "2606:4700::1", 443, true},
		{"[::ffff:127.0.0.1]:443", true, true, "::ffff:127.0.0.1", 443, true},
		{"[fe80::1%25en0]:443", true, true, "fe80::1%25en0", 443, true},
		{"[fe80::1%en0.1_A-b]:443", true, true, "fe80::1%en0.1_A-b", 443, true},
		{"[fe80::1%en 0]:443", false, false, "", 0, false},
		{"[fe80::1%en\n0]:443", false, false, "", 0, false},
		{"[fe80::1%en:0]:443", false, false, "", 0, false},
		{"[2606:4700::1]", false, true, "2606:4700::1", 0, true},
		{"example.com:65535", true, true, "example.com", 65535, false},
		{"example.com:1", true, true, "example.com", 1, false},
		{"", false, false, "", 0, false},
		{":443", false, false, "", 0, false},
		{"example.com:", false, false, "", 0, false},
		{"example.com:0", false, false, "", 0, false},
		{"example.com:0443", false, false, "", 0, false},
		{"example.com:+443", false, false, "", 0, false},
		{"example.com:65536", false, false, "", 0, false},
		{"example.com:443x", false, false, "", 0, false},
		{"example.com:443:443", false, false, "", 0, false},
		{"::1:443", false, false, "", 0, false}, // IPv6 without brackets
		{"[1.2.3.4]:443", false, false, "", 0, false},
		{"[github.com]:443", false, false, "", 0, false},
		{"[::1]x:443", false, false, "", 0, false},
		{"[::1:443", false, false, "", 0, false},
		{"exa mple.com:443", false, false, "", 0, false},
		{"a..b.com:443", false, false, "", 0, false},
		{".example.com:443", false, false, "", 0, false},
		{"user@example.com:443", false, false, "", 0, false},
		{"Key.com:443", false, false, "", 0, false},
		{"example.com/path:443", false, false, "", 0, false},
		{"127.1:443", false, false, "", 0, false},
		{"0x7f000001:80", false, false, "", 0, false},
		{"2130706433:443", false, false, "", 0, false},
		{"0177.0.0.1:443", false, false, "", 0, false},
		{"host.1abc:443", false, false, "", 0, false},
		{"localhost:8080", true, true, "localhost", 8080, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := ParseConnectTarget(tt.in)
			if ok != tt.connect {
				t.Fatalf("ParseConnectTarget(%q) ok = %v, want %v", tt.in, ok, tt.connect)
			}
			if ok && (got.Host != tt.wantHost || got.Port != tt.wantPort || got.IsIP() != tt.ip) {
				t.Errorf("ParseConnectTarget(%q) = %+v, want %s port %d ip %v", tt.in, got, tt.wantHost, tt.wantPort, tt.ip)
			}
			got, ok = ParseHostPort(tt.in, 80)
			if ok != tt.plain {
				t.Fatalf("ParseHostPort(%q) ok = %v, want %v", tt.in, ok, tt.plain)
			}
			wantPort := tt.wantPort
			if wantPort == 0 {
				wantPort = 80
			}
			if ok && (got.Host != tt.wantHost || got.Port != wantPort || got.IsIP() != tt.ip) {
				t.Errorf("ParseHostPort(%q) = %+v, want %s port %d ip %v", tt.in, got, tt.wantHost, wantPort, tt.ip)
			}
		})
	}
	if _, ok := ParseHostPort("example.com", 0); ok {
		t.Error("ParseHostPort with a zero default port accepted a host without one")
	}
	long := ""
	for range 64 {
		long += "a"
	}
	if _, ok := ParseConnectTarget(long + ".com:443"); ok {
		t.Error("a 64-byte label was accepted")
	}
}

func TestTargetString(t *testing.T) {
	for _, in := range []string{"github.com:443", "1.2.3.4:80", "[2606:4700::1]:443", "[fe80::1%25en0]:8443"} {
		got, ok := ParseConnectTarget(in)
		if !ok {
			t.Fatalf("ParseConnectTarget(%q) failed", in)
		}
		if got.String() != in {
			t.Errorf("String() = %q, want %q", got.String(), in)
		}
	}
}

func TestCheckHost(t *testing.T) {
	tests := []struct {
		header, connect string
		want            Code
	}{
		{"github.com", "github.com", ""},
		{"github.com:443", "github.com", ""},
		{"GitHub.COM", "github.com", ""},
		{"github.com.", "github.com", ""},
		{"github.com.:443", "github.com", ""},
		{"github.com", "GitHub.com.", ""},
		{"github.com:8443", "github.com", CodeHostMismatch},
		{"github.com:80", "github.com", CodeHostMismatch},
		{"api.github.com", "github.com", CodeHostMismatch},
		{"evil.example.com", "chatgpt.com", CodeHostMismatch},
		{"", "github.com", CodeHostMismatch},
		{"github.com:", "github.com", CodeHostMismatch},
		{"[::1]:443", "::1", CodeHostMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.header+"_"+tt.connect, func(t *testing.T) {
			if got := CheckHost(tt.header, tt.connect); got != tt.want {
				t.Errorf("CheckHost(%q, %q) = %q, want %q", tt.header, tt.connect, got, tt.want)
			}
		})
	}
}

// TestDecideConnect has a row per CONNECT row of the host table.
func TestDecideConnect(t *testing.T) {
	deny := Egress{Mode: ModeDenyByDefault, AllowHosts: []string{"*.githubusercontent.com", "codeload.github.com"}}
	tests := []struct {
		name   string
		e      Egress
		target string
		action Action
		code   Code
		class  Class
	}{
		{"anthropic", Egress{}, "api.anthropic.com:443", Intercept, "", Anthropic},
		{"claude.ai normalised", Egress{}, "CLAUDE.AI.:443", Intercept, "", Anthropic},
		{"console", Egress{}, "console.anthropic.com:443", Intercept, "", Anthropic},
		{"platform", Egress{}, "platform.claude.com:443", Intercept, "", Anthropic},
		{"chatgpt", Egress{}, "chatgpt.com:443", Intercept, "", ChatGPT},
		{"github", Egress{}, "github.com:443", Intercept, "", GitHub},
		{"github api", Egress{}, "api.github.com:443", Intercept, "", GitHubAPI},
		{"intercepted on another port", Egress{}, "github.com:22", Refuse, CodePort, GitHub},
		{"model host on 80", Egress{}, "chatgpt.com:80", Refuse, CodePort, ChatGPT},
		{"auth host", Egress{}, "auth.openai.com:443", Refuse, CodeAuthHost, AuthOpenAI},
		{"auth host, another port", Egress{}, "auth.openai.com:8443", Refuse, CodeAuthHost, AuthOpenAI},
		{"dispatcher", Egress{}, "dispatcher.factory.internal:80", Refuse, CodePlainOnly, Dispatcher},
		{"dispatcher on 443", Egress{}, "dispatcher.factory.internal:443", Refuse, CodePlainOnly, Dispatcher},
		{"api.openai.com tunnelled (D1)", Egress{}, "api.openai.com:443", Tunnel, "", Ordinary},
		{"ordinary, default", Egress{}, "downloads.claude.ai:443", Tunnel, "", Ordinary},
		{"ordinary, another port", Egress{}, "example.com:22", Tunnel, "", Ordinary},
		{"IP literal, default", Egress{}, "1.2.3.4:443", Tunnel, "", Ordinary},
		{"IPv6 literal, default", Egress{}, "[2606:4700::1]:443", Tunnel, "", Ordinary},
		{"deny-by-default, allowed", deny, "objects.githubusercontent.com:443", Tunnel, "", Ordinary},
		{"deny-by-default, exact", deny, "codeload.github.com:443", Tunnel, "", Ordinary},
		{"deny-by-default, not listed", deny, "downloads.claude.ai:443", Refuse, CodeEgressNotAllowed, Ordinary},
		{"deny-by-default, IP literal", deny, "1.2.3.4:443", Refuse, CodeEgressNotAllowed, Ordinary},
		{"deny-by-default keeps the git host", deny, "github.com:443", Intercept, "", GitHub},
		{"deny-by-default keeps the model", deny, "chatgpt.com:443", Intercept, "", ChatGPT},
		{"deny-by-default keeps the API", deny, "api.github.com:443", Intercept, "", GitHubAPI},
		{"no port", Egress{}, "github.com", Refuse, CodeBadTarget, Ordinary},
		{"empty", Egress{}, "", Refuse, CodeBadTarget, Ordinary},
		{"bad port", Egress{}, "github.com:0443", Refuse, CodeBadTarget, Ordinary},
		{"IPv6 without brackets", Egress{}, "::1:443", Refuse, CodeBadTarget, Ordinary},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DecideConnect(tt.target, tt.e)
			if d.Action != tt.action || d.Refusal != tt.code || d.Class != tt.class {
				t.Errorf("DecideConnect(%q) = %s %q %s, want %s %q %s", tt.target, d.Action, d.Refusal, d.Class, tt.action, tt.code, tt.class)
			}
			if (d.Action == Refuse) != (d.Refusal != "") {
				t.Errorf("DecideConnect(%q): action %s with refusal %q", tt.target, d.Action, d.Refusal)
			}
		})
	}
}

// TestDecidePlain has a row per plain HTTP row of the host table.
func TestDecidePlain(t *testing.T) {
	deny := Egress{Mode: ModeDenyByDefault, AllowHosts: []string{"mirror.example.com"}}
	tests := []struct {
		name         string
		e            Egress
		scheme, host string
		action       Action
		code         Code
		class        Class
		port         uint16
	}{
		{"ordinary", Egress{}, "http", "example.com", Forward, "", Ordinary, 80},
		{"ordinary with a port", Egress{}, "HTTP", "example.com:8080", Forward, "", Ordinary, 8080},
		{"IP literal", Egress{}, "http", "1.2.3.4", Forward, "", Ordinary, 80},
		{"https without CONNECT", Egress{}, "https", "example.com", Refuse, CodeNotProxy, Ordinary, 0},
		{"origin form", Egress{}, "", "", Refuse, CodeNotProxy, Ordinary, 0},
		{"no host", Egress{}, "http", "", Refuse, CodeNotProxy, Ordinary, 0},
		{"bad host", Egress{}, "http", "exa mple.com", Refuse, CodeNotProxy, Ordinary, 0},
		{"github", Egress{}, "http", "github.com", Refuse, CodePlainHTTP, GitHub, 80},
		{"github api", Egress{}, "http", "api.github.com", Refuse, CodePlainHTTP, GitHubAPI, 80},
		{"model host", Egress{}, "http", "api.anthropic.com", Refuse, CodePlainHTTP, Anthropic, 80},
		{"chatgpt on 443", Egress{}, "http", "chatgpt.com:443", Refuse, CodePlainHTTP, ChatGPT, 443},
		{"auth host", Egress{}, "http", "auth.openai.com", Refuse, CodeAuthHost, AuthOpenAI, 80},
		{"dispatcher", Egress{}, "http", "dispatcher.factory.internal", ToDispatcher, "", Dispatcher, 80},
		{"dispatcher on 80", Egress{}, "http", "dispatcher.factory.internal:80", ToDispatcher, "", Dispatcher, 80},
		{"dispatcher normalised", Egress{}, "http", "DISPATCHER.factory.internal.", ToDispatcher, "", Dispatcher, 80},
		{"dispatcher on another port", Egress{}, "http", "dispatcher.factory.internal:8800", Refuse, CodeDispatcherRoute, Dispatcher, 8800},
		{"deny-by-default, listed", deny, "http", "mirror.example.com", Forward, "", Ordinary, 80},
		{"deny-by-default, not listed", deny, "http", "example.com", Refuse, CodeEgressNotAllowed, Ordinary, 80},
		{"deny-by-default keeps the dispatcher", deny, "http", "dispatcher.factory.internal", ToDispatcher, "", Dispatcher, 80},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DecidePlain(tt.scheme, tt.host, tt.e)
			if d.Action != tt.action || d.Refusal != tt.code || d.Class != tt.class || d.Target.Port != tt.port {
				t.Errorf("DecidePlain(%q, %q) = %s %q %s port %d, want %s %q %s port %d",
					tt.scheme, tt.host, d.Action, d.Refusal, d.Class, d.Target.Port, tt.action, tt.code, tt.class, tt.port)
			}
		})
	}
	if s := Action(200).String(); s != "unknown" {
		t.Errorf("Action(200).String() = %q", s)
	}
}
