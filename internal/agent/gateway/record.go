package gateway

import (
	"encoding/json"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/compliance-framework/factory/internal/agent/gateway/policy"
)

// RecordType is a Record's transcript line type.
const RecordType = "factory.egress"

// maxPath is where a record's path is cut, on a UTF-8 boundary.
const maxPath = 512

// Mode says where a request was handled.
type Mode string

// The modes.
const (
	ModeConnect    Mode = "connect"    // a CONNECT to one of the gateway's own hosts
	ModeIntercept  Mode = "intercept"  // a request inside an intercepted CONNECT (PR 5)
	ModeTunnel     Mode = "tunnel"     // any other CONNECT: one record per tunnel, when it closes
	ModeForward    Mode = "forward"    // plain HTTP
	ModeDispatcher Mode = "dispatcher" // plain HTTP to the dispatcher
	ModeAccept     Mode = "accept"     // a peer outside Clients
)

// Record is one request, refusals included. It never holds a header value, a body, a query or a
// resolved address.
type Record struct {
	Type     string    // RecordType
	At       time.Time // the request's start, on the injected clock
	Mode     Mode
	Method   string
	Host     string // the target as policy normalised it; empty when it didn't parse
	Port     uint16
	Path     string // the URL path only, cut at 512 bytes; empty for CONNECT and tunnels
	Status   int    // the status the client got; 0 for a connection closed without one
	Injected string // the credential class added (Injected* constants, git:<owner>/<repo>) or ""
	Client   string // the VM's carriers: ClientNone, ClientPlaceholder or ClientOther; "" at accept
	Refusal  policy.Code
	Up, Down int64 // bytes from the client and to it (bodies, or a tunnel's streams)
	Duration time.Duration
}

type recordJSON struct {
	Type     string      `json:"type"`
	At       string      `json:"at"`
	Mode     Mode        `json:"mode"`
	Method   string      `json:"method"`
	Host     string      `json:"host"`
	Port     uint16      `json:"port"`
	Path     string      `json:"path"`
	Status   int         `json:"status"`
	Injected string      `json:"injected"`
	Client   string      `json:"client"`
	Refusal  policy.Code `json:"refusal"`
	Up       int64       `json:"up"`
	Down     int64       `json:"down"`
	MS       int64       `json:"ms"`
}

// MarshalJSON is the transcript line: gateway.md's factory.egress object.
func (r Record) MarshalJSON() ([]byte, error) {
	return json.Marshal(recordJSON{
		Type: r.Type, At: r.At.UTC().Format("2006-01-02T15:04:05.000Z07:00"), Mode: r.Mode,
		Method: r.Method, Host: r.Host, Port: r.Port, Path: r.Path, Status: r.Status,
		Injected: r.Injected, Client: r.Client, Refusal: r.Refusal, Up: r.Up, Down: r.Down,
		MS: r.Duration.Milliseconds(),
	})
}

// cutPath is p cut at maxPath bytes on a UTF-8 boundary.
func cutPath(p string) string {
	if len(p) <= maxPath {
		return p
	}
	i := maxPath
	for i > 0 && !utf8.RuneStart(p[i]) {
		i--
	}
	return p[:i]
}

// newRecord starts r's record.
func (g *Gateway) newRecord(r *http.Request, mode Mode, start time.Time) Record {
	rec := Record{At: start, Mode: mode, Method: r.Method, Client: classifyClient(r)}
	if r.Method != http.MethodConnect {
		rec.Path = cutPath(r.URL.Path)
	}
	return rec
}

// finish completes rec, logs it and records it.
func (g *Gateway) finish(rec Record) {
	rec.Duration = g.clk.Now().Sub(rec.At)
	if rec.Refusal != "" {
		g.log.Info("gateway: refused", "refusal", rec.Refusal, "mode", rec.Mode)
	}
	if rec.Client == ClientOther { // without the host or path, which the VM chose
		g.log.Warn("gateway: the VM sent a credential in a carrier", "mode", rec.Mode, "injected", rec.Injected)
	}
	g.log.Debug("gateway: request", "mode", rec.Mode, "method", rec.Method, "host", rec.Host,
		"port", rec.Port, "status", rec.Status, "refusal", rec.Refusal, "up", rec.Up, "down", rec.Down)
	g.record(rec)
}
