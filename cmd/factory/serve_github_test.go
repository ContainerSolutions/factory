package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/provider/github/githubtest"
)

const e2eWebhookSecret = "e2e-webhook-secret-0123456789"

// githubCatalog copies the PoC catalog with its github Provider pointed at api (githubtest).
func githubCatalog(t *testing.T, api string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(pocDir)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "providers", "github-public.yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), "apiURL: https://api.github.com", "apiURL: "+api, 1)
	if changed == string(data) {
		t.Fatal("the PoC Provider's apiURL moved")
	}
	if err := os.WriteFile(file, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeGitHub is githubtest as the PoC catalog's Provider expects it: the author (App 1) and
// reviewer (App 2) Apps installed on compliance-framework, gus an org admin (a maintainer),
// and the epic docs#42.
func fakeGitHub(t *testing.T) (*githubtest.Server, map[string]string) {
	t.Helper()
	srv := githubtest.NewServer(t, clock.Real{})
	authorKey, authorRSA := githubtest.AppKey(t)
	reviewerKey, reviewerRSA := githubtest.NewAppKey(t)
	srv.AddApp(1, &authorRSA.PublicKey)
	srv.AddApp(2, &reviewerRSA.PublicKey)
	srv.SetSlug(1, "ccf-factory")
	srv.SetSlug(2, "ccf-factory-reviewer")
	srv.AddInstallation(githubtest.Installation{ID: 11, AppID: 1, Owner: "compliance-framework", Permissions: map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read", "members": "read",
		"checks": "read", "actions": "read", "deployments": "read",
	}})
	srv.AddInstallation(githubtest.Installation{ID: 12, AppID: 2, Owner: "compliance-framework", Permissions: map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "read", "metadata": "read", "checks": "read",
	}})
	srv.AddOrgMember("compliance-framework", "gus", "admin")
	srv.AddObject(githubtest.Object{Repo: "compliance-framework/docs", Number: 42, Title: "Add i18n with a PT-BR translation",
		Body: "Translate the docs to PT-BR, one PR per section.", Author: githubtest.User{Login: "gus"}})
	env := testEnv()
	env["FACTORY_SECRET_GITHUB_PUBLIC_WEBHOOK_SECRET"] = e2eWebhookSecret
	env["FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY"] = string(authorKey)
	env["FACTORY_SECRET_CCF_FACTORY_REVIEWER_APP_PRIVATE_KEY"] = string(reviewerKey)
	return srv, env
}

// TestCatalogSourceToken: with --catalog-app-id, the git source mints a contents: read token
// for the catalog repository as that App; a bad private key fails at startup.
func TestCatalogSourceToken(t *testing.T) {
	ctx := context.Background()
	gh := githubtest.NewServer(t, clock.Real{})
	key, rsaKey := githubtest.NewAppKey(t)
	gh.AddApp(7, &rsaKey.PublicKey)
	gh.AddInstallation(githubtest.Installation{ID: 21, AppID: 7, Owner: "compliance-framework", Permissions: map[string]string{"contents": "read", "metadata": "read"}})
	host := strings.TrimPrefix(gh.URL, "http://")
	args := []string{"--state-memory", "--github-api-url", gh.URL, "--catalog-repo", "https://" + host + "/compliance-framework/factory-catalog",
		"--catalog-app-id", "7", "--catalog-app-key-ref", "ccf-factory-app/private-key"}
	log := slog.New(slog.DiscardHandler)

	for _, tt := range []struct {
		name    string
		pem     string
		wantErr string
	}{
		{name: "minted", pem: string(key)},
		{name: "bad private key", pem: "not a PEM", wantErr: "--catalog-app-id 7: tokens: identity \"catalog\""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := testEnv()
			env["FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY"] = tt.pem
			cfg, _, err := parseServeConfig(args, getenvOf(env), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			src, err := catalogSource(cfg, serveDeps{Clock: clock.Real{}}, log)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || strings.Contains(err.Error(), tt.pem) {
					t.Fatalf("catalogSource = %v, want %q (without the key)", err, tt.wantErr)
				}
				return
			}
			gs, ok := src.(controllers.GitSource)
			if err != nil || !ok || gs.Token == nil {
				t.Fatalf("catalogSource = %#v, %v; want a GitSource with a Token", src, err)
			}
			tok, err := gs.Token(ctx, "compliance-framework/factory-catalog")
			if err != nil || tok == "" || !slices.Contains(gh.Tokens(), tok) {
				t.Fatalf("Token = %q, %v; want a githubtest token", tok, err)
			}
			if strings.Contains(gs.String(), tok) {
				t.Errorf("String() = %q carries the token", gs.String())
			}
		})
	}
}

// postWebhook posts a GitHub delivery whose signature is signed's (the webhook secret's HMAC)
// and returns the status.
func postWebhook(t *testing.T, url, event, delivery string, body, signed []byte) int {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(e2eWebhookSecret))
	mac.Write(signed)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// TestServeGitHubEndToEnd is the fake-GitHub integration test RT-S2-T04 deferred: `factory
// serve` without --dev-fake-providers, its GitHub adapter against githubtest. A signed
// issues.opened by a maintainer becomes a plan-epic Task; a fake agent registered through the
// generated client leases it, reads its context (the issue, fenced), applies effects through
// the Effector (a comment, replayed by key without a second write; a sub-issue created and
// linked) and completes it; the effects, and the epic-plan notification of the rule that
// follows, land in githubtest.
func TestServeGitHubEndToEnd(t *testing.T) {
	ctx := context.Background()
	gh, env := fakeGitHub(t)
	users := fakeGitHubUsers(t)
	cfg, warnings, err := parseServeConfig([]string{
		"--state-memory", "--catalog-dir", githubCatalog(t, gh.URL),
		"--addr", "127.0.0.1:0", "--ops-addr", "localhost:0", "--public-url", "https://factory.test",
		"--github-api-url", users.URL, "--skills-base", "", "--shutdown-timeout", "5s", "--log-level", "debug",
	}, getenvOf(env), io.Discard)
	if err != nil || cfg.DevFakeProviders {
		t.Fatalf("config: %v (warnings %v)", err, warnings)
	}
	s := runServed(t, cfg, serveDeps{Getenv: getenvOf(env)})
	c := s.client(t)
	cli := bearer(testCLIToken)

	// Connected, and its health checked at connect (GET /app for both Apps).
	provs, err := c.ListProvidersWithResponse(ctx, &apiclient.ListProvidersParams{}, cli)
	if err != nil || provs.JSON200 == nil || len(provs.JSON200.Items) != 1 || provs.JSON200.Items[0].Health.Status != apiclient.ProviderHealthStatus(provider.HealthOK) {
		t.Fatalf("providers: %v %s %s", err, provs.Status(), provs.Body)
	}

	// A maintainer opens docs#42: GitHub delivers it signed, then redelivers it.
	body, err := os.ReadFile(filepath.Join("testdata", "serve", "github-issues-opened.json"))
	if err != nil {
		t.Fatal(err)
	}
	hook := s.public + "/hooks/" + ghProvider
	if code := postWebhook(t, hook, "issues", "72d3162e-cc78-11e3-81ab-4c9367dc0958", body, body); code != http.StatusAccepted {
		t.Fatalf("webhook: %d", code)
	}
	if code := postWebhook(t, hook, "issues", "72d3162e-cc78-11e3-81ab-4c9367dc0958", body, body); code != http.StatusOK {
		t.Fatalf("redelivery: %d", code)
	}
	if code := postWebhook(t, hook, "issues", "forged", append(body, ' '), body); code != http.StatusUnauthorized {
		t.Fatalf("forged delivery: %d", code)
	}

	// A fake agent (owner gus) registers and leases the plan-epic task.
	reg, err := c.RegisterAgentWithResponse(ctx, apiclient.RegisterAgentRequest{
		Pool: "local", Host: "e2e", AgentVersion: "0.1.0",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{
			Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}, ReasoningEffort: []string{"high"},
		}}},
		Lending: &v1alpha1.Lending{Queues: []string{"poc"}, MaxConcurrent: 1},
	}, enrolled(t, ctx, c, "e2e"))
	if err != nil || reg.JSON201 == nil {
		t.Fatalf("register: %v %s %s", err, reg.Status(), reg.Body)
	}
	agent := bearer(reg.JSON201.AgentToken)
	wait := "10s"
	var lease *apiclient.Lease
	eventuallyT(t, "a lease", func() bool {
		res, err := c.LeaseTaskWithResponse(ctx, reg.JSON201.AgentId, &apiclient.LeaseTaskParams{Wait: &wait}, agent)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode() != http.StatusOK && res.StatusCode() != http.StatusNoContent {
			t.Fatalf("lease: %s %s", res.Status(), res.Body)
		}
		lease = res.JSON200
		return lease != nil
	})
	task := lease.Task.Metadata.ID
	if spec := lease.Task.Spec.TaskSpec.Name; spec != "plan-epic" {
		t.Fatalf("leased %s, want plan-epic", spec)
	}
	// The git credential is a real installation token, on docs only.
	cred := lease.GitCredential
	if cred.Repo != "compliance-framework/docs" || !strings.HasPrefix(cred.Token, "ghs_") || cred.Permissions["issues"] != "write" {
		t.Errorf("git credential: repo %s, perms %v", cred.Repo, cred.Permissions)
	}

	// The context: the epic, its text read from GitHub and fenced as untrusted.
	n := -1
	for i, item := range lease.Task.Spec.Context {
		if item.Kind == provider.KindIssue && item.Ref == docsIssue {
			n = i
		}
	}
	if n < 0 {
		t.Fatalf("no context item for the epic: %+v", lease.Task.Spec.Context)
	}
	tc, err := c.GetTaskContextWithResponse(ctx, task, n, agent)
	if err != nil || tc.JSON200 == nil {
		t.Fatalf("context: %v %s %s", err, tc.Status(), tc.Body)
	}
	if got := tc.JSON200.Content; !strings.Contains(got, "untrusted source=") || !strings.Contains(got, "Translate the docs to PT-BR") ||
		!strings.Contains(got, `author="gus"`) || strings.Contains(got, "factory:") {
		t.Errorf("context content:\n%s", got)
	}

	// Effects through the Effector: a sub-issue created and linked, and the plan comment,
	// whose replay by key writes nothing.
	effect := func(op v1alpha1.EffectOp, key string, target *string, args map[string]any) *apiclient.EffectResponse {
		t.Helper()
		res, err := c.CreateEffectWithResponse(ctx, task, apiclient.EffectRequest{Op: op, IdempotencyKey: key, Target: target, Args: args}, agent)
		if err != nil || res.JSON200 == nil {
			t.Fatalf("effect %s: %v %s %s", op, err, res.Status(), res.Body)
		}
		return res.JSON200
	}
	created := effect(v1alpha1.OpCreateIssue, "plan-1", nil, map[string]any{"title": "Translate the intro"})
	if created.Url == nil || !strings.HasSuffix(*created.Url, "/compliance-framework/docs/issues/43") {
		t.Fatalf("create-issue: %+v", created)
	}
	epic := docsIssue
	effect(v1alpha1.OpSubIssueAdd, "plan-1-link", &epic, map[string]any{"issue": "#43"})
	first := effect(v1alpha1.OpComment, "plan-summary", nil, map[string]any{"body": "Plan: 1. #43"})
	comments := func() []githubtest.Comment {
		o, _ := gh.Object("compliance-framework/docs", 42)
		return o.Comments
	}
	before := len(comments())
	again := effect(v1alpha1.OpComment, "plan-summary", nil, map[string]any{"body": "Plan: 1. #43"})
	if again.EffectId != first.EffectId || len(comments()) != before {
		t.Errorf("replay: %+v after %+v, %d comments after %d", again, first, len(comments()), before)
	}
	if o, _ := gh.Object("compliance-framework/docs", 42); len(o.SubIssues) != 1 || o.SubIssues[0] != 43 {
		t.Errorf("sub-issues %v", o.SubIssues)
	}

	done, err := c.CompleteLeaseWithResponse(ctx, lease.Lease.Id, apiclient.CompleteRequest{Result: v1alpha1.Result{
		Task: task, Status: v1alpha1.ResultDone, Outcome: "decomposed", Summary: "one sub-issue",
		Harness: &v1alpha1.ResultHarness{Name: "codex", EffectiveConfig: v1alpha1.EffectiveConfig{Model: "gpt-5-codex"}},
		Data:    map[string]any{"plan": []any{map[string]any{"issue": "compliance-framework/docs#43", "title": "Translate the intro"}}},
	}}, agent)
	if err != nil || done.StatusCode() != http.StatusNoContent {
		t.Fatalf("complete: %v %s %s", err, done.Status(), done.Body)
	}

	// On GitHub: the factory's projection marker, the agent's plan comment and, from
	// build-stack-start, the epic-plan notification, all by the author App.
	has := func(pred func(githubtest.Comment) bool) bool {
		for _, cm := range comments() {
			if cm.Author.Login == "ccf-factory[bot]" && pred(cm) {
				return true
			}
		}
		return false
	}
	eventuallyT(t, "the plan comment and the epic-plan notification on docs#42", func() bool {
		return has(func(cm githubtest.Comment) bool {
			return strings.HasPrefix(cm.Body, "Plan: 1. #43\n\n<!-- factory:effect eff_")
		}) &&
			has(func(cm githubtest.Comment) bool {
				return strings.Contains(cm.Body, "compliance-framework/docs#43") && strings.Contains(cm.Body, "<!-- factory:effect evt")
			}) &&
			has(func(cm githubtest.Comment) bool { return strings.HasPrefix(cm.Body, "<!-- factory:wi v=1 ") })
	})
	gt, err := c.GetTaskWithResponse(ctx, task, cli)
	if err != nil || gt.JSON200 == nil || gt.JSON200.Status.Phase != v1alpha1.TaskSucceeded {
		t.Fatalf("task after complete: %v %s %s", err, gt.Status(), gt.Body)
	}

	if err := s.stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	logs := s.logs.String()
	secrets := append([]string{testCLIToken, testJoinToken, e2eWebhookSecret, reg.JSON201.AgentToken,
		"BEGIN RSA PRIVATE KEY"}, gh.Tokens()...)
	for _, secret := range secrets {
		if strings.Contains(logs, secret) {
			t.Errorf("a secret is in the logs: %q", secret)
		}
	}
	if !strings.Contains(logs, `"msg":"provider health","provider":"github-public","status":"ok"`) {
		t.Errorf("no health check logged")
	}
}
