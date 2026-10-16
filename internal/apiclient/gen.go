// Package apiclient is the oapi-codegen client for api/openapi.yaml, used by factory-agent and
// the factory CLI. Payloads that are v1alpha1 kinds are the internal/apis/v1alpha1 types
// (x-go-type), not copies.
//
// Generated: never edit client.gen.go by hand; run make generate.
package apiclient

//go:generate go tool oapi-codegen -config config.yaml ../../api/openapi.yaml
