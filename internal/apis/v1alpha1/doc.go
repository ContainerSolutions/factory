// Package v1alpha1 holds the Go types for every factory kind: the config kinds that live in
// git under catalog/ (Provider, Group, Template, Rule, RuleSet, TaskSpec, Queue, Pool), the
// runtime kinds the dispatcher stores (Event, WorkItem, Task, Run, Agent, Effect,
// BudgetCounter, Approval), the agent's Result, the capability descriptors, the event
// vocabulary and the effect ops.
//
// These types are the contract between workstreams (design/workstreams/foundation/LLD/contracts.md).
// Change them in their own PR. The JSON Schemas under schemas/ are generated from them by
// ./schemagen (make schemas).
//
// Decoding is strict: Decode rejects unknown fields, unknown kinds and a foreign apiVersion.
// Values are kept as written (durations, patterns, templated values) so that a decoded
// document marshals back to the same document; checking them is the catalog loader's job.
package v1alpha1

// APIVersion is the only apiVersion this package accepts. Documents may omit it.
const APIVersion = "factory.ccf.dev/v1alpha1"
