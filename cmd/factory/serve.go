package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/provider/github"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/store/objectstore"
)

// Environment variables `factory serve` reads (api-server.md §factory serve). Secrets come from
// the environment only, never from flags, and are never logged.
const (
	envPublicURL         = "FACTORY_PUBLIC_URL"
	envOAuthClientID     = "FACTORY_OAUTH_CLIENT_ID"
	envOAuthClientSecret = "FACTORY_OAUTH_CLIENT_SECRET"
	envSessionKey        = "FACTORY_SESSION_KEY"
	// envSessionKeyPrevious is the key before the last rotation, still accepted when
	// verifying sessions and OAuth states.
	envSessionKeyPrevious = "FACTORY_SESSION_KEY_PREVIOUS"
	envCLIToken           = "FACTORY_CLI_TOKEN"
	// envSecretPrefix starts the variable of a catalog secretRef: FACTORY_SECRET_<NAME>_<KEY>.
	envSecretPrefix = "FACTORY_SECRET_"
)

// minSecretLen is the shortest session key and CLI token accepted (`openssl rand -hex 32`
// gives 64 characters).
const minSecretLen = 32

// Defaults of `factory serve`.
const (
	defaultAddr            = ":8800"
	defaultOpsAddr         = "127.0.0.1:9090"
	defaultRetention       = 2160 * time.Hour // 90 days
	defaultCatalogTimeout  = 2 * time.Minute
	defaultShutdownTimeout = 30 * time.Second
	defaultGitHubURL       = "https://github.com"
	defaultSkillsBase      = "https://github.com/compliance-framework"
)

// serveConfig is `factory serve`'s configuration, from flags and the environment.
type serveConfig struct {
	Addr      string
	OpsAddr   string
	PublicURL string

	// The catalog: a git repository (Repo, Ref, Path) or a local directory (Dir).
	CatalogRepo     string
	CatalogRef      string
	CatalogPath     string
	CatalogDir      string
	CatalogInterval time.Duration
	CatalogTimeout  time.Duration

	Retention       time.Duration
	ShutdownTimeout time.Duration

	// The store: one of StateDir (the file store, one dispatcher), StateURL (the object store,
	// shared by every dispatcher on it: gs://, s3:// or file://) and StateMemory (in memory,
	// lost on restart). --dev-fake-providers alone is StateMemory too.
	StateDir    string
	StateURL    string
	StateMemory bool
	// StateSyncInterval is the object store's sync interval (--state-url only).
	StateSyncInterval time.Duration

	// DevFakeProviders registers the in-memory fake adapter for the github type, for
	// development and tests only.
	DevFakeProviders bool

	// AuthProvider is the catalog Provider the UI's and agent owners' GitHub logins belong to
	// (default: the catalog's only github Provider).
	AuthProvider string
	// GitHubURL serves the OAuth web flow; GitHubAPIURL serves GET /user (default: the auth
	// Provider's spec.github.apiURL).
	GitHubURL    string
	GitHubAPIURL string
	// SkillsBase resolves TaskSpec skill refs with git ls-remote; empty keeps them unresolved.
	SkillsBase string
	// DeployEnvironment is the GitHub environment whose latest deployment of the default
	// branch the GitHub adapter reports for merged PRs (item.deploy).
	DeployEnvironment string
	// AntiAffinityWait is the scheduler's Options.AntiAffinityWait: how long a review waits
	// before its author's agent may take it on the author's harness; --anti-affinity-wait 0
	// is scheduler.AntiAffinityOff.
	AntiAffinityWait time.Duration

	LogLevel  slog.Level
	LogFormat string

	// Secrets, from the environment only.
	OAuthClientID     string
	OAuthClientSecret string
	SessionKey        string
	// SessionKeyPrevious is FACTORY_SESSION_KEY_PREVIOUS: accepted for verifying (rotation).
	SessionKeyPrevious string
	CLIToken           string
}

// errHelp is returned by parseServeConfig for -h.
var errHelp = errors.New("help")

// parseServeConfig reads the flags in args and the environment through getenv. It returns the
// config, warnings to log, and an error naming every invalid setting (never a secret's value).
func parseServeConfig(args []string, getenv func(string) string, stderr io.Writer) (serveConfig, []string, error) {
	var cfg serveConfig
	fl := flag.NewFlagSet("factory serve", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.StringVar(&cfg.Addr, "addr", defaultAddr, "public listener: the UI, /api/v1 and /hooks (what the tunnel forwards to)")
	fl.StringVar(&cfg.OpsAddr, "ops-addr", defaultOpsAddr, "ops listener: /healthz, /readyz, /metrics (no auth: must be a loopback address)")
	fl.StringVar(&cfg.PublicURL, "public-url", getenv(envPublicURL), "public base URL (the tunnel): OAuth callback, webhook URLs, UI links (env "+envPublicURL+")")
	fl.StringVar(&cfg.CatalogRepo, "catalog-repo", controllers.DefaultCatalogRepo, "catalog git repository")
	fl.StringVar(&cfg.CatalogRef, "catalog-ref", controllers.DefaultCatalogRef, "catalog branch, tag or commit")
	fl.StringVar(&cfg.CatalogPath, "catalog-path", controllers.DefaultCatalogPath, "catalog directory inside the repository")
	fl.StringVar(&cfg.CatalogDir, "catalog-dir", "", "load the catalog from this local directory instead of git (development)")
	fl.DurationVar(&cfg.CatalogInterval, "catalog-interval", controllers.DefaultCatalogPollInterval, "how often the catalog source is polled")
	fl.DurationVar(&cfg.CatalogTimeout, "catalog-timeout", defaultCatalogTimeout, "how long startup waits for the first valid catalog")
	fl.DurationVar(&cfg.Retention, "retention", defaultRetention, "how long runs, events, effects and finished tasks (and closed work items) are kept")
	fl.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", defaultShutdownTimeout, "how long shutdown waits for in-flight requests")
	fl.StringVar(&cfg.StateDir, "state-dir", "", "keep the dispatcher's state in this directory (0700, created if missing), so it survives restarts; one dispatcher only")
	fl.StringVar(&cfg.StateURL, "state-url", "", "keep the dispatcher's state in object storage, shared by every dispatcher on it: gs://bucket/prefix, s3://bucket/prefix or file:///dir (never logged)")
	fl.BoolVar(&cfg.StateMemory, "state-memory", false, "keep the state in memory: every work item starts fresh on a restart, with no state and no run counters (tests and quick local runs)")
	fl.DurationVar(&cfg.StateSyncInterval, "state-sync-interval", objectstore.DefaultSyncInterval, "with --state-url: how stale a read may be before it syncs with the shared state, and the background sync's period")
	fl.BoolVar(&cfg.DevFakeProviders, "dev-fake-providers", false, "DEVELOPMENT ONLY: connect github Providers to the in-memory fake adapter (no GitHub calls)")
	fl.StringVar(&cfg.AuthProvider, "auth-provider", "", "catalog Provider of the UI and agent-owner logins (default: the catalog's only github Provider)")
	fl.StringVar(&cfg.GitHubURL, "github-url", defaultGitHubURL, "GitHub web URL for the OAuth flow")
	fl.StringVar(&cfg.GitHubAPIURL, "github-api-url", "", "GitHub API URL for GET /user (default: the auth Provider's spec.github.apiURL)")
	fl.StringVar(&cfg.SkillsBase, "skills-base", defaultSkillsBase, "base URL of skill repositories, resolved with git ls-remote; empty: leave skill refs unresolved")
	fl.StringVar(&cfg.DeployEnvironment, "deploy-environment", github.DefaultDeployEnvironment, "GitHub environment whose deployments of the default branch are item.deploy")
	fl.DurationVar(&cfg.AntiAffinityWait, "anti-affinity-wait", scheduler.DefaultAntiAffinityWait, "how long a review waits for another agent before the author's agent may take it on the author's harness; 0 turns anti-affinity off")
	level := fl.String("log-level", "info", "log level: debug, info, warn or error")
	fl.StringVar(&cfg.LogFormat, "log-format", "json", "log format: json or text")
	fl.Usage = func() { serveUsage(stderr, fl) }
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return cfg, nil, errHelp
		}
		return cfg, nil, err
	}
	if fl.NArg() > 0 {
		return cfg, nil, fmt.Errorf("unexpected arguments %v", fl.Args())
	}
	set := map[string]bool{}
	fl.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg.OAuthClientID = getenv(envOAuthClientID)
	cfg.OAuthClientSecret = getenv(envOAuthClientSecret)
	cfg.SessionKey = getenv(envSessionKey)
	cfg.SessionKeyPrevious = getenv(envSessionKeyPrevious)
	cfg.CLIToken = getenv(envCLIToken)

	var errs, warnings []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if err := cfg.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		bad("--log-level must be debug, info, warn or error, not %q", *level)
	}
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		bad("--log-format must be json or text, not %q", cfg.LogFormat)
	}

	// Listeners.
	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		bad("--addr %q: %v", cfg.Addr, err)
	}
	if host, _, err := net.SplitHostPort(cfg.OpsAddr); err != nil {
		bad("--ops-addr %q: %v", cfg.OpsAddr, err)
	} else if !isLoopback(host) {
		bad("--ops-addr %q must be a loopback address (127.0.0.1, ::1 or localhost): the ops routes have no auth and must never be public", cfg.OpsAddr)
	}
	if cfg.OpsAddr == cfg.Addr {
		bad("--ops-addr must differ from --addr")
	}

	// Public URL.
	if cfg.PublicURL == "" {
		_, port, _ := net.SplitHostPort(cfg.Addr)
		cfg.PublicURL = "http://localhost:" + port
		warnings = append(warnings, "no --public-url or "+envPublicURL+": using "+cfg.PublicURL+"; OAuth callbacks and webhook URLs only work locally")
	} else if u, err := url.Parse(cfg.PublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		bad("--public-url %q must be an absolute http(s) URL without credentials, query or fragment", cfg.PublicURL)
	} else {
		cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
		if u.Scheme == "http" && !isLoopback(u.Hostname()) {
			warnings = append(warnings, "--public-url is http: session cookies are Secure, so the UI login needs https")
		}
	}

	// Catalog source.
	if cfg.CatalogDir != "" {
		for _, f := range []string{"catalog-repo", "catalog-ref", "catalog-path"} {
			if set[f] {
				bad("--catalog-dir and --%s are exclusive: the catalog comes from a directory or from git", f)
			}
		}
		if info, err := os.Stat(cfg.CatalogDir); err != nil || !info.IsDir() {
			bad("--catalog-dir %q is not a directory", cfg.CatalogDir)
		}
	} else {
		if cfg.CatalogRepo == "" || cfg.CatalogRef == "" || cfg.CatalogPath == "" {
			bad("--catalog-repo, --catalog-ref and --catalog-path must not be empty")
		}
		if strings.HasPrefix(cfg.CatalogRef, "-") {
			bad("--catalog-ref %q must not start with '-'", cfg.CatalogRef)
		}
	}

	// The store: a durable one, unless asked otherwise (P3 §No rebuild from markers).
	switch {
	case cfg.StateDir != "" && cfg.StateURL != "":
		bad("--state-dir and --state-url are exclusive: the state is in a directory (one dispatcher) or in object storage (shared)")
	case cfg.StateMemory && (cfg.StateDir != "" || cfg.StateURL != ""):
		bad("--state-memory and --state-dir or --state-url are exclusive")
	case cfg.StateDir == "" && cfg.StateURL == "" && !cfg.StateMemory && !cfg.DevFakeProviders:
		bad("serve needs a durable store: --state-dir DIR, or --state-url URL for several dispatchers (--state-memory keeps it in memory, " +
			"for tests and quick local runs: every work item then starts fresh on a restart)")
	}
	if cfg.StateDir != "" {
		if info, err := os.Stat(cfg.StateDir); err == nil && !info.IsDir() {
			bad("--state-dir %q is not a directory", cfg.StateDir)
		}
	}
	if cfg.StateSyncInterval <= 0 {
		bad("--state-sync-interval must be positive")
	}
	if set["state-sync-interval"] && cfg.StateURL == "" {
		warnings = append(warnings, "--state-sync-interval applies to --state-url only; ignored")
	}

	if strings.TrimSpace(cfg.DeployEnvironment) == "" {
		bad("--deploy-environment must not be empty")
	}

	// Durations.
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"retention", cfg.Retention}, {"catalog-interval", cfg.CatalogInterval},
		{"catalog-timeout", cfg.CatalogTimeout}, {"shutdown-timeout", cfg.ShutdownTimeout},
	} {
		if d.v <= 0 {
			bad("--%s must be positive", d.name)
		}
	}

	switch {
	case cfg.AntiAffinityWait < 0:
		bad("--anti-affinity-wait must not be negative (0 turns anti-affinity off)")
	case cfg.AntiAffinityWait == 0:
		cfg.AntiAffinityWait = scheduler.AntiAffinityOff
	}

	// GitHub URLs.
	for _, u := range []struct{ name, v string }{{"github-url", cfg.GitHubURL}, {"github-api-url", cfg.GitHubAPIURL}, {"skills-base", cfg.SkillsBase}} {
		if u.v == "" && u.name != "github-url" {
			continue
		}
		if p, err := url.Parse(u.v); err != nil || p.Scheme == "" || p.Host == "" {
			bad("--%s %q must be an absolute URL", u.name, u.v)
		}
	}
	cfg.GitHubURL = strings.TrimRight(cfg.GitHubURL, "/")
	cfg.GitHubAPIURL = strings.TrimRight(cfg.GitHubAPIURL, "/")

	// Secrets: names only in messages.
	var missing []string
	for _, s := range []struct{ env, v string }{
		{envOAuthClientID, cfg.OAuthClientID}, {envOAuthClientSecret, cfg.OAuthClientSecret},
		{envSessionKey, cfg.SessionKey}, {envCLIToken, cfg.CLIToken},
	} {
		if s.v == "" && (s.env != envSessionKey || cfg.StateURL == "") {
			missing = append(missing, s.env)
		}
	}
	if len(missing) > 0 {
		bad("missing environment variables: %s", strings.Join(missing, ", "))
	}
	if cfg.StateURL != "" && cfg.SessionKey == "" {
		bad("--state-url needs %s: every dispatcher on the shared state must sign and verify the session cookies and the login states with the same key", envSessionKey)
	}
	if cfg.SessionKey != "" && len(cfg.SessionKey) < minSecretLen {
		bad("%s must be at least %d bytes (openssl rand -hex 32)", envSessionKey, minSecretLen)
	}
	if cfg.SessionKeyPrevious != "" && len(cfg.SessionKeyPrevious) < minSecretLen {
		bad("%s must be at least %d bytes (openssl rand -hex 32)", envSessionKeyPrevious, minSecretLen)
	}
	if cfg.CLIToken != "" && len(cfg.CLIToken) < minSecretLen {
		bad("%s must be at least %d characters (openssl rand -hex 32)", envCLIToken, minSecretLen)
	}

	if cfg.DevFakeProviders {
		warnings = append(warnings, "--dev-fake-providers: github Providers are connected to the in-memory fake adapter; nothing reaches GitHub (development only)")
		if cfg.StateDir == "" && cfg.StateURL == "" {
			cfg.StateMemory = true
		}
	}
	if len(errs) > 0 {
		return cfg, warnings, errors.New(strings.Join(errs, "; "))
	}
	return cfg, warnings, nil
}

// isLoopback reports whether host is a loopback name or IP (an empty host listens on every
// interface, so it isn't).
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func serveUsage(w io.Writer, fl *flag.FlagSet) {
	fmt.Fprintln(w, `usage: factory serve [--addr :8800] [--ops-addr 127.0.0.1:9090] [--public-url URL]
                     [--catalog-repo URL --catalog-ref main --catalog-path catalog | --catalog-dir DIR]
                     [--retention 2160h] [--state-dir DIR | --state-url URL | --state-memory]
                     [--deploy-environment github-pages]
                     [--anti-affinity-wait 10m]
                     [--log-level info] [--log-format json|text] [--dev-fake-providers]

Runs the dispatcher: the store, catalog-sync, the provider connections, the controllers, the
scheduler, the API server with the embedded UI on --addr, and the ops routes on --ops-addr.
The store is durable: --state-dir keeps it in files there (one dispatcher), --state-url in
object storage that every dispatcher on it shares (one of them, the leader, runs the
controllers). --state-memory keeps it in memory, where every work item starts fresh on a
restart; --dev-fake-providers implies it.

environment (secrets come from the environment only and are never logged):
  `+envOAuthClientID+`, `+envOAuthClientSecret+`   UI login (GitHub OAuth App)
  `+envSessionKey+`   session cookie and login state signing key (at least 32 bytes; the
      same on every dispatcher of a --state-url)
  `+envSessionKeyPrevious+`   the key before a rotation, still accepted (optional)
  `+envCLIToken+`   CLI admin token, for factory dispatch (at least 32 characters)
  `+envPublicURL+`   public base URL (not a secret), same as --public-url
  `+envSecretPrefix+`<NAME>_<KEY>   a catalog secretRef {name, key}, upper-cased with
      every other character as '_' (e.g. FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN)

flags:`)
	fl.PrintDefaults()
}

func serveCommand() command {
	return command{
		name:    "serve",
		summary: "run the dispatcher (API, UI, controllers, scheduler)",
		run:     runServeCommand,
	}
}

func runServeCommand(ctx context.Context, args []string, _, stderr io.Writer) int {
	cfg, warnings, err := parseServeConfig(args, os.Getenv, stderr)
	if errors.Is(err, errHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "factory serve: %v\n", err)
		return exitUsage
	}
	log := newLogger(cfg, stderr)
	slog.SetDefault(log)
	for _, w := range warnings {
		log.Warn(w)
	}
	if err := runServe(ctx, cfg, serveDeps{Getenv: os.Getenv}, log); err != nil {
		log.Error("factory serve failed", "err", err)
		return 1
	}
	return 0
}

// newLogger returns the slog logger cfg asks for, writing to w.
func newLogger(cfg serveConfig, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
