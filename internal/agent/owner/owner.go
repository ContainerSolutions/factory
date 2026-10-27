// Package owner gets the agent owner's GitHub user token for registration (supervise.md
// §Registration): a GitHub OAuth device flow against the dispatcher's OAuth client, whose
// id comes from GET /api/v1/auth/device-config. The owner approves in their browser; the
// token is sent once in POST /agents and dropped. No gh login is used on the worker's OS user.
package owner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// TokenSource returns a fresh owner token, once per registration.
type TokenSource interface {
	OwnerToken(ctx context.Context) (string, error)
}

// TokenFunc adapts a function to TokenSource (tests).
type TokenFunc func(ctx context.Context) (string, error)

// OwnerToken calls f.
func (f TokenFunc) OwnerToken(ctx context.Context) (string, error) { return f(ctx) }

// GitHub's device-flow grant type and error codes
// (docs.github.com, "Authorizing OAuth apps", device flow).
const (
	grantDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"

	errPending   = "authorization_pending"
	errSlowDown  = "slow_down"
	errExpired   = "expired_token"
	errDenied    = "access_denied"
	slowDownStep = 5 * time.Second
)

// Errors of the device flow.
var (
	ErrDenied  = errors.New("the owner denied the device login")
	ErrExpired = errors.New("the device code expired before the owner approved it")
)

// DeviceFlow runs GitHub's OAuth device flow.
type DeviceFlow struct {
	// API is the dispatcher client (GET /auth/device-config gives the client id and scopes).
	API *apiclient.ClientWithResponses
	// GitHubURL is the GitHub web host, e.g. https://github.com.
	GitHubURL string
	// HTTPClient calls GitHub. Default: a client with a 30 s timeout.
	HTTPClient *http.Client
	// Clock paces the polling. Default clock.Real.
	Clock clock.Clock
	// Prompt shows the owner where to approve. Default: nothing (tests).
	Prompt func(verificationURI, userCode string, expiresIn time.Duration)
}

type deviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
	Interval    int    `json:"interval"`
}

// OwnerToken runs the flow: fetch the client config, request a device code, show the owner
// the user code, and poll until they approve, deny, or the code expires.
func (d *DeviceFlow) OwnerToken(ctx context.Context) (string, error) {
	clk := d.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	hc := d.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if d.API == nil {
		return "", errors.New("device flow: no dispatcher client")
	}
	cfgRes, err := d.API.GetDeviceConfigWithResponse(ctx)
	if err != nil {
		return "", fmt.Errorf("device flow: get device config: %w", err)
	}
	if cfgRes.JSON200 == nil || cfgRes.JSON200.ClientId == "" {
		return "", fmt.Errorf("device flow: get device config: %s", cfgRes.Status())
	}
	cfg := cfgRes.JSON200
	base := strings.TrimRight(d.GitHubURL, "/")

	var dc deviceCode
	if err := postForm(ctx, hc, base+"/login/device/code", url.Values{
		"client_id": {cfg.ClientId},
		"scope":     {strings.Join(cfg.Scopes, " ")},
	}, &dc); err != nil {
		return "", fmt.Errorf("device flow: request a device code: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" || dc.VerificationURI == "" {
		return "", errors.New("device flow: GitHub returned an incomplete device code")
	}
	expiresIn := time.Duration(dc.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 15 * time.Minute
	}
	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if d.Prompt != nil {
		d.Prompt(dc.VerificationURI, dc.UserCode, expiresIn)
	}
	deadline := clk.Now().Add(expiresIn)
	for {
		t := clk.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C():
		}
		if !clk.Now().Before(deadline) {
			return "", ErrExpired
		}
		var tr tokenResponse
		if err := postForm(ctx, hc, base+"/login/oauth/access_token", url.Values{
			"client_id":   {cfg.ClientId},
			"device_code": {dc.DeviceCode},
			"grant_type":  {grantDeviceCode},
		}, &tr); err != nil {
			return "", fmt.Errorf("device flow: poll for the token: %w", err)
		}
		switch tr.Error {
		case "":
			if tr.AccessToken == "" {
				return "", errors.New("device flow: GitHub returned no token")
			}
			return tr.AccessToken, nil
		case errPending:
		case errSlowDown:
			interval += slowDownStep
			if tr.Interval > 0 {
				interval = time.Duration(tr.Interval) * time.Second
			}
		case errExpired:
			return "", ErrExpired
		case errDenied:
			return "", ErrDenied
		default:
			return "", fmt.Errorf("device flow: %s: %s", tr.Error, tr.Description)
		}
	}
}

// postForm posts a form asking for JSON and decodes the answer into out. Bodies (which carry
// codes and tokens) are never included in errors.
func postForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", endpoint, res.Status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: undecodable answer", endpoint)
	}
	return nil
}
