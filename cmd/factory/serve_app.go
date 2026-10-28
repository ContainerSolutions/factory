package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/auth"
	"github.com/ContainerSolutions/factory/internal/catalog"
	"github.com/ContainerSolutions/factory/internal/catalog/gitsource"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/fake"
	"github.com/ContainerSolutions/factory/internal/provider/github"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/server"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/webui"
)

// githubHTTPTimeout bounds each call to GitHub's OAuth and user endpoints.
const githubHTTPTimeout = 30 * time.Second

// healthCheckTimeout bounds a Provider's health check (GitHub: GET /app per identity).
const healthCheckTimeout = 15 * time.Second

// recoverSlack is how far before its own start an instance recovers deliveries: GitHub's
// webhook timeout, so a delivery that timed out while the instance started is covered.
const recoverSlack = 10 * time.Second

// serveDeps are runServe's injectable dependencies; the zero value is production.
type serveDeps struct {
	// Getenv reads the catalog secretRefs (FACTORY_SECRET_<NAME>_<KEY>). Default: none set.
	Getenv func(string) string
	// Clock. Default clock.Real.
	Clock clock.Clock
	// Fake is the fake adapter --dev-fake-providers registers (a new one when nil), so a test
	// can seed it.
	Fake *fake.Adapter
	// HTTPClient calls GitHub (OAuth, GET /user). Default a client with a 30 s timeout.
	HTTPClient *http.Client
	// Listening, when set, gets the bound public and ops addresses once both listen.
	Listening func(public, ops net.Addr)
	// Ready, when set, is called once startup is done (listeners and controllers up).
	Ready func()
	// Leader configures the leader lock (the zero value takes its defaults); ResyncInterval
	// is the work-item resync's default interval. Tests shorten them.
	Leader         controllers.LeaderOptions
	ResyncInterval time.Duration
}

// adapters are the provider adapters `factory serve` registers: the GitHub adapter, unless
// --dev-fake-providers puts the fake in its place.
func adapters(cfg serveConfig, d serveDeps, groups provider.GroupLookup, log *slog.Logger) []provider.Adapter {
	var out []provider.Adapter
	if cfg.DevFakeProviders {
		a := d.Fake
		if a == nil {
			a = fake.NewAdapter(fake.Options{Clock: d.Clock, Groups: groups})
		}
		out = append(out, a)
	} else {
		out = append(out, healthChecked{Adapter: github.NewAdapter(github.Options{
			HTTPClient: d.HTTPClient, Clock: d.Clock, Logger: log.With("component", "github"), Groups: groups,
			UIBaseURL: cfg.PublicURL, DeployEnvironment: cfg.DeployEnvironment,
		}), log: log})
	}
	return out
}

// healthChecker is a Conn that checks its own health (the GitHub Conn's CheckHealth); Health
// alone only reports what was last seen.
type healthChecker interface {
	CheckHealth(ctx context.Context) provider.HealthStatus
}

// healthChecked runs a Conn's health check whenever its adapter connects one: at startup and
// on every reconnect after a catalog change.
type healthChecked struct {
	provider.Adapter
	log *slog.Logger
}

func (h healthChecked) Connect(ctx context.Context, p v1alpha1.Provider, secrets provider.SecretResolver) (provider.Conn, error) {
	c, err := h.Adapter.Connect(ctx, p, secrets)
	if err == nil {
		checkHealth(ctx, p.Metadata.Name, c, h.log)
	}
	return c, err
}

// checkHealth runs c's health check, when it has one, and logs the result. A failing check
// doesn't fail anything: Health reports it (the API's provider health) until it recovers.
func checkHealth(ctx context.Context, name string, c provider.Conn, log *slog.Logger) {
	hc, ok := c.(healthChecker)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	st := hc.CheckHealth(cctx)
	if st.Status == provider.HealthOK {
		log.Info("provider health", "provider", name, "status", st.Status)
		return
	}
	log.Warn("provider health", "provider", name, "status", st.Status, "message", st.Message)
}

// followHealth re-checks every connected Provider's health whenever the catalog changes (a
// resync), until ctx is done; Providers the change reconnected were checked by healthChecked.
func followHealth(ctx context.Context, cs *controllers.CatalogSync, pc *controllers.ProviderController, log *slog.Logger) error {
	_, changed := cs.Snapshot()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}
		_, changed = cs.Snapshot()
		for _, name := range pc.Names() {
			if c, ok := pc.Conn(name); ok {
				checkHealth(ctx, name, c, log)
			}
		}
	}
}

// envSecrets resolves a catalog secretRef from FACTORY_SECRET_<NAME>_<KEY>.
type envSecrets func(string) string

// secretEnv is the variable of a secretRef: FACTORY_SECRET_ + NAME + _ + KEY, upper-cased, every
// character but A-Z and 0-9 replaced by '_'.
func secretEnv(ref v1alpha1.SecretRef) string {
	norm := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z':
				return r - 'a' + 'A'
			case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				return r
			}
			return '_'
		}, s)
	}
	return envSecretPrefix + norm(ref.Name) + "_" + norm(ref.Key)
}

// Resolve implements provider.SecretResolver. The error names the variable, never a value.
func (e envSecrets) Resolve(_ context.Context, ref v1alpha1.SecretRef) ([]byte, error) {
	name := secretEnv(ref)
	v := e(name)
	if v == "" {
		return nil, fmt.Errorf("secret %s/%s: %s is not set", ref.Name, ref.Key, name)
	}
	return []byte(v), nil
}

// runGroup runs the long-lived components and reports the first one that fails.
type runGroup struct {
	wg   sync.WaitGroup
	errc chan error
}

func newRunGroup() *runGroup { return &runGroup{errc: make(chan error, 1)} }

// Go runs f(ctx); an error while ctx is live is reported on errc (the first one only).
func (g *runGroup) Go(ctx context.Context, name string, f func(context.Context) error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := f(ctx); err != nil && ctx.Err() == nil {
			select {
			case g.errc <- fmt.Errorf("%s: %w", name, err):
			default:
			}
		}
	}()
}

// runServe runs the dispatcher until ctx is done, then shuts it down gracefully. Startup order
// (controllers.md §Ordering and startup): store → the leader lock (tried at once, then every
// minute) → catalog-sync (wait for the first good catalog) → providers (connect, recover the
// deliveries since 10 s before this process started) → listeners (webhooks are stored as
// Deliveries from now on) → the provider (normalise), rule, debounce, work-item, run
// controllers, the scheduler, the sweeper and the installation check → ready. Every instance
// serves the API and runs the scheduler's tick; the controllers' reconciles, the lease
// controller, Refresh, the sweeper and the installation check run on the leader only (P3
// §Several dispatchers). There is no rebuild from markers: the store is the authority.
func runServe(ctx context.Context, cfg serveConfig, d serveDeps, log *slog.Logger) error {
	if d.Getenv == nil {
		d.Getenv = func(string) string { return "" }
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{Timeout: githubHTTPTimeout}
	}
	clk := d.Clock
	startedAt := clk.Now()
	secrets := envSecrets(d.Getenv)

	log.Info("factory serve starting",
		"addr", cfg.Addr, "opsAddr", cfg.OpsAddr, "publicURL", cfg.PublicURL,
		"catalog", catalogSource(cfg).String(), "retention", cfg.Retention.String(),
		"stateDir", cfg.StateDir, "stateURL", stateURLScheme(cfg.StateURL), "stateMemory", cfg.StateMemory,
		"devFakeProviders", cfg.DevFakeProviders)

	// The store, first: it is closed last, once everything that writes to it has stopped
	// (shutdown closes it within the shutdown timeout; this covers a failed start).
	st, err := openStore(ctx, cfg, store.Options{Clock: clk, Retention: cfg.Retention, Logger: log}, log)
	if err != nil {
		return err
	}
	storeClosed := false
	closeOnce := func(ctx context.Context) error {
		if storeClosed {
			return nil
		}
		storeClosed = true
		return closeStore(ctx, st)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		if err := closeOnce(cctx); err != nil {
			log.Warn("store not closed cleanly", "err", err)
		}
	}()

	// The leader lock: given up once the loops have stopped, before the store closes.
	lopts := d.Leader
	lopts.Clock, lopts.Logger = clk, log
	leader := controllers.NewLeader(st, lopts)
	defer leader.Release()

	// Components run under runCtx, which is cancelled only once the listeners have drained,
	// so in-flight completes still reach the run controller.
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	group := newRunGroup()
	defer group.wg.Wait()
	defer cancelRun()
	group.Go(runCtx, "leader", leader.Run)

	// Catalog.
	cs := controllers.NewCatalogSync(catalogSource(cfg), controllers.CatalogSyncOptions{
		Interval: cfg.CatalogInterval, Clock: clk, Logger: log,
	})
	group.Go(runCtx, "catalog-sync", cs.Run)
	if err := waitCatalog(ctx, cs, cfg.CatalogTimeout); err != nil {
		return err
	}
	cat := cs.Current()
	log.Info("catalog loaded", "sha", cat.SHA, "providers", len(cat.Providers), "ruleSets", len(cat.RuleSets))

	// Providers.
	groups := func(name string) (v1alpha1.GroupSpec, bool) {
		c := cs.Current()
		if c == nil {
			return v1alpha1.GroupSpec{}, false
		}
		g, ok := c.Groups[name]
		if !ok {
			return v1alpha1.GroupSpec{}, false
		}
		return g.Spec, true
	}
	reg, err := provider.NewRegistry(adapters(cfg, d, groups, log)...)
	if err != nil {
		return fmt.Errorf("provider registry: %w", err)
	}
	pc, err := controllers.NewProviderController(st, cs, reg, controllers.ProviderOptions{Clock: clk, Logger: log, Secrets: secrets})
	if err != nil {
		return err
	}
	if err := pc.Connect(ctx); err != nil {
		return fmt.Errorf("connect providers: %w", err)
	}
	for _, name := range pc.Names() {
		log.Info("provider connected", "provider", name, "type", cat.Providers[name].Spec.Type,
			"webhook", cfg.PublicURL+"/hooks/"+name)
	}
	group.Go(runCtx, "provider-health", func(ctx context.Context) error { return followHealth(ctx, cs, pc, log) })
	installs := controllers.NewInstallationCheck(pc, controllers.InstallationCheckOptions{SkillsBase: cfg.SkillsBase, Logger: log})
	group.Go(runCtx, "installations", func(ctx context.Context) error {
		leader.While(ctx, "installations", func(ctx context.Context) error { return installs.Follow(ctx, cs) })
		return nil
	})
	// Every instance recovers what GitHub sent while it started: a create of each Delivery,
	// deduplicated by id, so it is the same intake as a webhook on any instance.
	if err := pc.Recover(ctx, startedAt.Add(-recoverSlack)); err != nil {
		log.Warn("recovering deliveries failed; continuing", "err", err)
	}

	// Controllers, scheduler, auth and the API server (constructed; started below).
	authProvider, apiBase, err := githubAuth(cfg, cat)
	if err != nil {
		return err
	}
	factory := controllers.NewFactoryProvider(st, clk)
	cache := controllers.NewObservationCache(pc, clk, 0)
	var skills controllers.SkillResolver = controllers.KeepSkills{}
	if cfg.SkillsBase != "" {
		gs := &controllers.GitSkills{Base: cfg.SkillsBase}
		if !cfg.DevFakeProviders { // the fake's tokens open nothing: resolve anonymously
			gs.Token = skillsToken(cs.Current, pc, cfg.SkillsBase, authProvider)
		}
		skills = gs
	}
	rule, err := controllers.NewRuleController(st, cs, pc, controllers.RuleOptions{
		Clock: clk, Logger: log, Cache: cache, Skills: skills, UIBaseURL: cfg.PublicURL,
	})
	if err != nil {
		return err
	}
	dbn, err := controllers.NewDebounceController(rule)
	if err != nil {
		return err
	}
	wic, err := controllers.NewWorkItemController(st, cs, pc, factory, controllers.WorkItemOptions{
		Clock: clk, Logger: log, Cache: cache, Retention: cfg.Retention, ResyncInterval: d.ResyncInterval,
	})
	if err != nil {
		return err
	}
	runs, err := controllers.NewRunController(st, cs, factory, controllers.RunOptions{Clock: clk, Logger: log})
	if err != nil {
		return err
	}
	sched, err := scheduler.New(scheduler.Deps{
		Store: st, Catalog: cs, Conns: pc, Runs: runs,
	}, scheduler.Options{Clock: clk, Logger: log, AntiAffinityWait: cfg.AntiAffinityWait, Leader: leader})
	if err != nil {
		return err
	}
	// The controllers that write from a watch or a timer reconcile on the leader only.
	for _, c := range []*controllers.Controller{pc.Controller(), rule.Controller(), dbn.Controller(), wic.Controller(), runs.Controller()} {
		if err := c.SetGate(leader); err != nil {
			return err
		}
	}
	authn, err := auth.New(auth.Config{
		CLIToken: cfg.CLIToken,
		OAuth: auth.OAuthConfig{
			ClientID: cfg.OAuthClientID, ClientSecret: cfg.OAuthClientSecret,
			AuthorizeURL: cfg.GitHubURL + "/login/oauth/authorize",
			TokenURL:     cfg.GitHubURL + "/login/oauth/access_token",
			APIBase:      apiBase,
			RedirectURL:  cfg.PublicURL + server.BasePath + "/auth/callback",
		},
		Provider:           authProvider,
		SessionKey:         []byte(cfg.SessionKey),
		PreviousSessionKey: []byte(cfg.SessionKeyPrevious),
		HTTPClient:         d.HTTPClient,
		Clock:              clk,
		Logger:             log,
	}, auth.Deps{Store: st, Catalog: cs, Conns: pc, Agents: sched})
	if err != nil {
		return err
	}
	repo, ref := "", ""
	if cfg.CatalogDir == "" {
		repo, ref = gitsource.RedactURL(cfg.CatalogRepo), cfg.CatalogRef
	}
	srv, err := server.New(server.Deps{
		Store: st, Transcripts: st, Catalog: cs, CatalogStatus: cs, Resync: cs, Installations: installs,
		Conns: pc, Sink: pc, Scheduler: sched, Factory: factory,
		KillSwitches: controllers.StoreKillSwitches{Store: st}, Evaluator: rule, Replayer: rule, Auth: authn, Skills: skills,
	}, server.Options{
		PublicURL: cfg.PublicURL, CatalogRepo: repo, CatalogRef: ref,
		Controllers: []*controllers.Controller{
			cs.Controller(), pc.Controller(), rule.Controller(), dbn.Controller(),
			wic.Controller(), runs.Controller(), sched.Controller(),
		},
		Intake: pc,
		Clock:  clk, Logger: log,
	})
	if err != nil {
		return err
	}
	ui := echo.WrapHandler(webui.Handler())
	srv.Echo().GET("/*", ui)
	srv.Echo().HEAD("/*", ui)

	// Listeners.
	pubL, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on --addr: %w", err)
	}
	opsL, err := net.Listen("tcp", cfg.OpsAddr)
	if err != nil {
		_ = pubL.Close()
		return fmt.Errorf("listen on --ops-addr: %w", err)
	}
	errLog := slog.NewLogLogger(log.With("component", "http").Handler(), slog.LevelWarn)
	pubConns, opsConns := newConnTracker(), newConnTracker()
	public := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, ErrorLog: errLog, ConnState: pubConns.track}
	ops := &http.Server{Handler: srv.OpsHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, ErrorLog: errLog, ConnState: opsConns.track}
	serveErr := make(chan error, 2)
	var serving sync.WaitGroup
	for _, l := range []struct {
		s     *http.Server
		l     net.Listener
		conns *connTracker
	}{{public, pubL, pubConns}, {ops, opsL, opsConns}} {
		serving.Add(1)
		go func() {
			defer serving.Done()
			err := l.s.Serve(l.l)
			if errors.Is(err, http.ErrServerClosed) {
				// The listener stopped (Shutdown): a connection that never sent a request
				// won't, and Shutdown would wait for it.
				l.conns.closeFresh()
				return
			}
			if err != nil {
				serveErr <- err
			}
		}()
	}
	log.Info("listening", "addr", pubL.Addr().String(), "opsAddr", opsL.Addr().String())
	if d.Listening != nil {
		d.Listening(pubL.Addr(), opsL.Addr())
	}

	shutdown := func(reason string) error {
		log.Info("shutting down", "reason", reason)
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		var errs []error
		// SSE streams and lease long polls end first, or Shutdown would wait for them.
		if err := srv.Shutdown(sctx); err != nil {
			errs = append(errs, fmt.Errorf("end streams: %w", err))
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, s := range []*http.Server{public, ops} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.Shutdown(sctx); err != nil {
					_ = s.Close()
					mu.Lock()
					errs = append(errs, fmt.Errorf("drain requests: %w", err))
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		serving.Wait()
		cancelRun()
		group.wg.Wait()
		leader.Release()
		// Nothing writes to the store any more: close it (the final compaction, then the
		// bucket).
		if err := closeOnce(sctx); err != nil {
			errs = append(errs, err)
		}
		if err := errors.Join(errs...); err != nil {
			log.Warn("shutdown incomplete", "err", err)
			return err
		}
		log.Info("shutdown complete")
		return nil
	}

	// The controllers (their reconciles wait for the leader lock) and the leader's sweep: the
	// store's retention, then the records it can't age out on its own.
	sweeps := leaderSweeps{st: st, authn: authn, wic: wic, log: log.With("component", "sweeper")}
	group.Go(runCtx, "sweeper", func(ctx context.Context) error {
		leader.While(ctx, "sweeper", func(ctx context.Context) error {
			store.RunSweeper(ctx, sweeps, 0, sweeps.log)
			return nil
		})
		return nil
	})
	group.Go(runCtx, "provider", pc.Run)
	group.Go(runCtx, "rule", rule.Run)
	group.Go(runCtx, "debounce", dbn.Run)
	group.Go(runCtx, "work-item", wic.Run)
	group.Go(runCtx, "run", runs.Run)
	group.Go(runCtx, "scheduler", sched.Run)
	log.Info("ready")
	if d.Ready != nil {
		d.Ready()
	}

	select {
	case <-ctx.Done():
		return shutdown("signal")
	case err := <-serveErr:
		_ = shutdown("listener failed")
		return fmt.Errorf("serve: %w", err)
	case err := <-group.errc:
		_ = shutdown("component failed")
		return err
	}
}

// leaderSweeps is the leader's sweep: the store's own (retention, and the object store's
// compaction, lock records and transcript orphans), then the records nothing else ages out:
// used login states past their expiry and resync baselines whose work item is gone.
type leaderSweeps struct {
	st    store.Sweeper
	authn *auth.Authenticator
	wic   *controllers.WorkItemController
	log   *slog.Logger
}

// Sweep implements store.Sweeper.
func (l leaderSweeps) Sweep(ctx context.Context) (store.SweepResult, error) {
	res, err := l.st.Sweep(ctx)
	if n, uerr := l.authn.SweepUsedStates(ctx); uerr != nil {
		err = errors.Join(err, uerr)
	} else if n > 0 {
		l.log.DebugContext(ctx, "swept used login states", "deleted", n)
	}
	if n, oerr := l.wic.SweepObservations(ctx); oerr != nil {
		err = errors.Join(err, oerr)
	} else if n > 0 {
		l.log.InfoContext(ctx, "swept resync baselines of deleted work items", "deleted", n)
	}
	return res, err
}

// connTracker follows a server's connections that haven't sent a request yet (StateNew), so
// shutdown can close them once the listener stops: http.Server.Shutdown counts such a
// connection as busy for 5 s, which can outlast --shutdown-timeout.
type connTracker struct {
	mu      sync.Mutex
	fresh   map[net.Conn]struct{}
	stopped bool
}

func newConnTracker() *connTracker { return &connTracker{fresh: map[net.Conn]struct{}{}} }

// track is http.Server.ConnState.
func (t *connTracker) track(c net.Conn, st http.ConnState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st != http.StateNew {
		delete(t.fresh, c)
		return
	}
	if t.stopped {
		_ = c.Close()
		return
	}
	t.fresh[c] = struct{}{}
}

// closeFresh closes the connections that haven't sent a request, and any accepted later.
func (t *connTracker) closeFresh() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	for c := range t.fresh {
		_ = c.Close()
	}
	clear(t.fresh)
}

// catalogSource is the catalog-sync source cfg names.
func catalogSource(cfg serveConfig) controllers.CatalogSource {
	if cfg.CatalogDir != "" {
		return controllers.NewDirSource(cfg.CatalogDir, catalog.Options{})
	}
	return controllers.GitSource{Repo: cfg.CatalogRepo, Ref: cfg.CatalogRef, Path: cfg.CatalogPath}
}

// waitCatalog waits for catalog-sync's first good catalog, at most timeout.
func waitCatalog(ctx context.Context, cs *controllers.CatalogSync, timeout time.Duration) error {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-cs.Ready():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for the catalog: %w", ctx.Err())
	case <-t.C:
	}
	st := cs.Status()
	var why []string
	if st.SourceError != "" {
		why = append(why, "source: "+st.SourceError)
	}
	for i, e := range st.Errors {
		if i == 3 {
			why = append(why, fmt.Sprintf("… %d more", len(st.Errors)+st.ErrorsOmitted-3))
			break
		}
		why = append(why, e.Error())
	}
	if len(why) == 0 {
		why = append(why, "no valid catalog loaded yet")
	}
	return fmt.Errorf("no valid catalog from %s after %s: %s", st.Source, timeout, strings.Join(why, "; "))
}

// githubAuth returns the catalog Provider the GitHub logins belong to and the API base for
// GET /user: --auth-provider (else the catalog's only github Provider), and --github-api-url
// (else that Provider's spec.github.apiURL, else api.github.com).
func githubAuth(cfg serveConfig, cat *catalog.Catalog) (string, string, error) {
	name := cfg.AuthProvider
	if name == "" {
		var gh []string
		for n, p := range cat.Providers {
			if p.Spec.Type == v1alpha1.ProviderTypeGitHub {
				gh = append(gh, n)
			}
		}
		slices.Sort(gh)
		if len(gh) != 1 {
			return "", "", fmt.Errorf("the catalog has %d github Providers %v: set --auth-provider", len(gh), gh)
		}
		name = gh[0]
	}
	p, ok := cat.Providers[name]
	if !ok {
		return "", "", fmt.Errorf("--auth-provider %q is not a catalog Provider", name)
	}
	if p.Spec.Type != v1alpha1.ProviderTypeGitHub {
		return "", "", fmt.Errorf("--auth-provider %q is of type %q, not github", name, p.Spec.Type)
	}
	api := cfg.GitHubAPIURL
	if api == "" && p.Spec.GitHub != nil {
		api = strings.TrimRight(p.Spec.GitHub.APIURL, "/")
	}
	if api == "" {
		api = auth.DefaultAPIBase
	}
	return name, api, nil
}

// Skills tokens: the identity they are minted as and their TTL (one ls-remote each, cached by
// url@ref).
const (
	skillsIdentity = "author"
	skillsTokenTTL = 10 * time.Minute
)

// skillsToken is GitSkills.Token: a read-only token (contents: read, that repo only) for a
// skills repo, minted as the author identity on the connection of the github Provider whose
// git host is the skills base's (the auth Provider when several are). The Provider is looked
// up at each call, so a catalog change applies. So the author App must be installed on a
// private skills repo for refs to resolve; the agent's fetch uses a read-only token of the
// TaskSpec's identity (credentials.git.readRepos), so the reviewer App needs it too.
func skillsToken(current func() *catalog.Catalog, conns controllers.ConnSource, base, preferred string) func(context.Context, string) (string, error) {
	host := ""
	if u, err := url.Parse(base); err == nil {
		host = u.Host
	}
	return func(ctx context.Context, repo string) (string, error) {
		name, err := skillsProvider(current(), host, preferred)
		if err != nil {
			return "", err
		}
		conn, ok := conns.Conn(name)
		if !ok {
			return "", fmt.Errorf("no connection for provider %q", name)
		}
		cred, err := conn.MintGitCredential(ctx, provider.ReadOnlyRequest(skillsIdentity, repo, skillsTokenTTL))
		if err != nil {
			return "", fmt.Errorf("provider %s as %s: %w", name, skillsIdentity, err)
		}
		return cred.Token, nil
	}
}

// skillsProvider is the github Provider whose git host is host: the only one, else preferred
// when it is one of them.
func skillsProvider(cat *catalog.Catalog, host, preferred string) (string, error) {
	var names []string
	if cat != nil {
		for n, p := range cat.Providers {
			if h := controllers.GitHost(p); h != "" && strings.EqualFold(h, host) {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	switch {
	case len(names) == 1:
		return names[0], nil
	case slices.Contains(names, preferred):
		return preferred, nil
	case len(names) == 0:
		return "", fmt.Errorf("no github Provider has the skills base's git host %q", host)
	}
	return "", fmt.Errorf("github Providers %v share the skills base's git host %q and none is the auth Provider", names, host)
}
