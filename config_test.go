package main

import (
	"sync"
	"testing"
)

// Regression test: the config watcher republishes hot-reloadable fields from
// its own goroutine while request handlers read them. Publication must be
// synchronized. Run with -race to catch the unsynchronized field writes.
func TestConfigHotReloadPublicationIsSynchronized(t *testing.T) {
	cfg := &config{}
	cfg.setHotReloadable(0.5, RoutingConfigFile{}, map[string]ClientPolicy{"initial": {}}, ExperimentsConfig{})

	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < 500; i++ {
			cfg.setHotReloadable(
				float64(i%100)/100,
				RoutingConfigFile{DefaultProfile: "quota_saver"},
				map[string]ClientPolicy{string(rune('a' + i%26)): {}},
				ExperimentsConfig{Canary: map[string]CanaryConfig{string(rune('a' + i%26)): {}}},
			)
		}
	}()

	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 500; j++ {
				if policies := cfg.hotClientPolicies(); policies == nil {
					t.Error("client policies vanished during hot reload")
					return
				}
				_ = cfg.hotTierThreshold()
				_ = cfg.hotRouting()
				_ = cfg.hotExperiments()
			}
		}()
	}
	readers.Wait()
	writers.Wait()
}
