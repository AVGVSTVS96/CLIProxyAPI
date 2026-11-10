package registry

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForModelCatalogsHoldsUntilFirstLoad(t *testing.T) {
	oldGeneral, oldCodex, oldDevin := generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater = oldGeneral, oldCodex, oldDevin
	}()

	release := make(chan struct{})
	var published atomic.Bool
	generalCatalogUpdater = &catalogUpdater{
		fetch: func(ctx context.Context, source string) ([]byte, error) {
			<-release
			return []byte(source), nil
		},
		publish: func([]byte) ([]string, error) { published.Store(true); return nil, nil },
	}
	codexCatalogUpdater = &catalogUpdater{}
	devinCatalogUpdater = &catalogUpdater{}

	generalCatalogUpdater.configure(ctx, "https://example.com/general")
	codexCatalogUpdater.configure(ctx, "disabled")
	devinCatalogUpdater.configure(ctx, "disabled")

	if WaitForModelCatalogs(ctx, 20*time.Millisecond) {
		t.Fatal("wait returned before the general catalog loaded")
	}
	close(release)
	if !WaitForModelCatalogs(ctx, 5*time.Second) {
		t.Fatal("wait did not return after the general catalog loaded")
	}
	if !published.Load() {
		t.Fatal("wait returned before the catalog was published")
	}
}

func TestWaitForModelCatalogsNeverConfigured(t *testing.T) {
	oldGeneral, oldCodex, oldDevin := generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater
	defer func() {
		generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater = oldGeneral, oldCodex, oldDevin
	}()
	generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater = &catalogUpdater{}, &catalogUpdater{}, &catalogUpdater{}
	if !WaitForModelCatalogs(context.Background(), time.Second) {
		t.Fatal("unconfigured catalogs should not hold startup")
	}
}
