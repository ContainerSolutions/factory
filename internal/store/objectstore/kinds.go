package objectstore

import (
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

// The object store's own internal kinds. They are written through the log like any other
// object, so every dispatcher on the bucket sees them. Neither holds a secret.
const (
	// KindStoreLock is a held lock (store.Store's Lock), named after the lock.
	KindStoreLock = "StoreLock"
	// KindTranscriptBlob indexes one transcript blob (or environment log), named after it.
	KindTranscriptBlob = "TranscriptBlob"
)

// StoreLock is a lock held across dispatchers: who holds it until when, on the holder's
// store clock. Lock creates it, or takes it over with a compare-and-swap once it has expired;
// Renew updates it; Unlock deletes it at the version its holder last wrote.
type StoreLock struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	// Holder is the holding store's instance id.
	Holder string `json:"holder"`
	// Token is random per Lock: it tells two holds by one instance apart in the log.
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

var _ store.MetaObject = (*StoreLock)(nil)

// GetName returns metadata.name: the lock's name.
func (l *StoreLock) GetName() string { return l.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (l *StoreLock) GetObjectMeta() *v1alpha1.ObjectMeta { return &l.Metadata }

// TranscriptBlob is a transcript blob's size and when it was last put, on the store's clock:
// the caps, eviction (oldest PutAt first) and retention read them from memory, since a bucket
// listing can't give them on every backend. createdAt can't serve, because an update keeps it
// and a replaced blob counts as new.
type TranscriptBlob struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	Size     int64               `json:"size"`
	PutAt    time.Time           `json:"putAt"`
}

var _ store.MetaObject = (*TranscriptBlob)(nil)

// GetName returns metadata.name: the blob's run name (store.TranscriptStore's).
func (b *TranscriptBlob) GetName() string { return b.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (b *TranscriptBlob) GetObjectMeta() *v1alpha1.ObjectMeta { return &b.Metadata }

// ownKinds are registered with every object store.
func ownKinds() map[string]store.Object {
	return map[string]store.Object{
		KindStoreLock:      &StoreLock{},
		KindTranscriptBlob: &TranscriptBlob{},
	}
}
