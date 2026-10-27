package store

// The schema of the stored kinds: one number for every stored type (the v1alpha1 runtime
// kinds and the internal kinds), bumped by any change to a stored kind or its fields. A shared
// log (an object store, P3) carries the writer's SchemaVersion on every change and snapshot, so
// two releases can run on one log during a deploy without losing writes.
//
// Expand, then write. A change to stored types ships in two releases:
//
//  1. Release N adds the new kind or optional field to the Go types, and bumps ReadableSchema
//     to N+1. It still writes N.
//  2. Release N+1 bumps SchemaVersion to N+1.
//
// So during a deploy, and after a rollback from N+1 to N, every instance can read every entry
// and keep the fields it rewrites. While ReadableSchema is pending (SchemaVersion+1), further
// changes accumulate into it, until a release writes it by bumping SchemaVersion. A store that meets an entry past its ReadableSchema stops
// writing (ErrReadOnly); one whose newest snapshot is past it refuses to start.
//
// cmd/factory's TestStoredSchema keeps these honest: a golden of every stored kind's JSON
// fields fails when a stored type changes without a pending ReadableSchema.
//
// Design: P3 §Two releases on one log; store.md §Interface, and §Object store when present.
const (
	// SchemaVersion is the schema this release writes.
	SchemaVersion = 1
	// ReadableSchema is the newest schema this release reads without losing data: SchemaVersion
	// or SchemaVersion+1.
	ReadableSchema = 1
)
