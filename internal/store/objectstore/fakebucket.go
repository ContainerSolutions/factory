package objectstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ContainerSolutions/factory/internal/clock"
)

// FakeOp is a Bucket call, as FakeBucket's fault hook sees it.
type FakeOp string

// Bucket calls.
const (
	FakeGet    FakeOp = "get"
	FakeCreate FakeOp = "create"
	FakeList   FakeOp = "list"
	FakeDelete FakeOp = "delete"
)

// Fault is what FakeBucket does with a call.
type Fault int

// Faults.
const (
	// Pass does the call.
	Pass Fault = iota
	// Fail fails the call without doing it (ErrInjected).
	Fail
	// LandedTimedOut does the call, then fails it as a timeout would (ErrInjected wrapping
	// context.DeadlineExceeded): the caller can't tell whether it happened.
	LandedTimedOut
	// LandedTaken does a Create, then answers ErrExists: an SDK that retried a create after its
	// first attempt landed.
	LandedTaken
)

// ErrInjected is the error of a call FakeBucket was told to fail.
var ErrInjected = errors.New("objectstore: injected fault")

// FakeRequest is one call, as FakeBucket's fault hook sees it.
type FakeRequest struct {
	Op FakeOp
	// Name is the object (the prefix for a List).
	Name string
}

type fakeObject struct {
	data    []byte
	updated time.Time
}

// FakeBucket is an in-memory Bucket for tests: objects stamped with a clock, a count of each
// call, and a hook that can fail calls, report them as timed out after they landed, or race
// them (the hook may write to the bucket, through another store, before the call goes on).
type FakeBucket struct {
	clk clock.Clock

	mu      sync.Mutex
	objects map[string]fakeObject
	calls   map[FakeOp]int
	faults  func(FakeRequest) Fault
}

var _ Bucket = (*FakeBucket)(nil)

// NewFakeBucket returns an empty bucket whose objects' Updated times come from clk.
func NewFakeBucket(clk clock.Clock) *FakeBucket {
	if clk == nil {
		clk = clock.Real{}
	}
	return &FakeBucket{clk: clk, objects: map[string]fakeObject{}, calls: map[FakeOp]int{}}
}

// SetFaults sets the hook every call goes through first (nil: every call passes). The hook is
// called without the bucket's lock, so it may call the bucket, or a store on it.
func (b *FakeBucket) SetFaults(f func(FakeRequest) Fault) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults = f
}

// Calls returns how many calls of op the bucket has had (faulted ones included).
func (b *FakeBucket) Calls(op FakeOp) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[op]
}

// Names returns the names of the objects under prefix, sorted.
func (b *FakeBucket) Names(prefix string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for n := range b.objects {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// Objects returns a copy of every object's bytes, by name.
func (b *FakeBucket) Objects() map[string][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string][]byte, len(b.objects))
	for n, o := range b.objects {
		out[n] = slices.Clone(o.data)
	}
	return out
}

// SetObject writes an object as is, bypassing the fault hook (tests plant objects with it).
func (b *FakeBucket) SetObject(name string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[name] = fakeObject{data: slices.Clone(data), updated: b.clk.Now()}
}

// fault counts the call and asks the hook.
func (b *FakeBucket) fault(ctx context.Context, op FakeOp, name string) (Fault, error) {
	b.mu.Lock()
	b.calls[op]++
	f := b.faults
	b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Pass, err
	}
	if f == nil {
		return Pass, nil
	}
	switch fl := f(FakeRequest{Op: op, Name: name}); fl {
	case Fail:
		return fl, fmt.Errorf("%s %s: %w", op, name, ErrInjected)
	default:
		return fl, nil
	}
}

func timedOut(op FakeOp, name string) error {
	return fmt.Errorf("%s %s: %w: %w", op, name, ErrInjected, context.DeadlineExceeded)
}

// Get implements Bucket.
func (b *FakeBucket) Get(ctx context.Context, name string) ([]byte, error) {
	fl, err := b.fault(ctx, FakeGet, name)
	if err != nil {
		return nil, err
	}
	if fl == LandedTimedOut {
		return nil, timedOut(FakeGet, name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objects[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, ErrNotExist)
	}
	return slices.Clone(o.data), nil
}

// Create implements Bucket.
func (b *FakeBucket) Create(ctx context.Context, name string, data []byte) error {
	fl, err := b.fault(ctx, FakeCreate, name)
	if err != nil {
		return err
	}
	b.mu.Lock()
	_, taken := b.objects[name]
	if !taken {
		b.objects[name] = fakeObject{data: slices.Clone(data), updated: b.clk.Now()}
	}
	b.mu.Unlock()
	switch {
	case fl == LandedTimedOut:
		return timedOut(FakeCreate, name)
	case taken || fl == LandedTaken:
		return fmt.Errorf("%s: %w", name, ErrExists)
	}
	return nil
}

// List implements Bucket. fn is called without the bucket's lock, on the listing as it was
// when the call began.
func (b *FakeBucket) List(ctx context.Context, prefix, startAfter string, fn func(ObjectInfo) error) error {
	fl, err := b.fault(ctx, FakeList, prefix)
	if err != nil {
		return err
	}
	if fl == LandedTimedOut {
		return timedOut(FakeList, prefix)
	}
	b.mu.Lock()
	objs := make([]ObjectInfo, 0, len(b.objects))
	for _, n := range slices.Sorted(maps.Keys(b.objects)) {
		if strings.HasPrefix(n, prefix) && n > startAfter {
			o := b.objects[n]
			objs = append(objs, ObjectInfo{Name: n, Size: int64(len(o.data)), Updated: o.updated})
		}
	}
	b.mu.Unlock()
	for _, o := range objs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(o); err != nil {
			return err
		}
	}
	return nil
}

// Delete implements Bucket.
func (b *FakeBucket) Delete(ctx context.Context, name string) error {
	fl, err := b.fault(ctx, FakeDelete, name)
	if err != nil {
		return err
	}
	b.mu.Lock()
	_, ok := b.objects[name]
	delete(b.objects, name)
	b.mu.Unlock()
	if fl == LandedTimedOut {
		return timedOut(FakeDelete, name)
	}
	if !ok {
		return fmt.Errorf("%s: %w", name, ErrNotExist)
	}
	return nil
}
