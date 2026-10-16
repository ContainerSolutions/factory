// Package gen is the oapi-codegen Echo strict server for api/openapi.yaml: the
// StrictServerInterface the dispatcher's handlers implement (internal/server), its
// request/response types and RegisterHandlers. Payloads that are v1alpha1 kinds are the
// internal/apis/v1alpha1 types (x-go-type), not copies.
//
// Generated: never edit server.gen.go by hand; run make generate.
package gen

//go:generate go tool oapi-codegen -config config.yaml ../../../api/openapi.yaml
