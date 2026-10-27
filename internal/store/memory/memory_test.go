package memory_test

import (
	"testing"

	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
	"github.com/ContainerSolutions/factory/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(o store.Options) storetest.Subject { return memory.New(o) })
}
