/*
Copyright 2026 The Kubernetes Authors All rights reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package discovery

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// appendToMapBuggy reproduces the pre-fix AppendToMap behaviour for comparison.
func appendToMapBuggy(r *CRDiscoverer, gvkps ...groupVersionKindPlural) {
	if r.Map == nil {
		r.Map = map[string]map[string][]kindPlural{}
	}
	if r.GVKToReflectorStopChanMap == nil {
		r.GVKToReflectorStopChanMap = map[string]chan struct{}{}
	}
	for _, gvkp := range gvkps {
		if _, ok := r.Map[gvkp.Group]; !ok {
			r.Map[gvkp.Group] = map[string][]kindPlural{}
		}
		if _, ok := r.Map[gvkp.Group][gvkp.Version]; !ok {
			r.Map[gvkp.Group][gvkp.Version] = []kindPlural{}
		}
		r.Map[gvkp.Group][gvkp.Version] = append(r.Map[gvkp.Group][gvkp.Version], kindPlural{Kind: gvkp.Kind, Plural: gvkp.Plural})
		r.GVKToReflectorStopChanMap[gvkp.GroupVersionKind.String()] = make(chan struct{})
	}
}

func makeGVKPs(n int) []groupVersionKindPlural {
	gvkps := make([]groupVersionKindPlural, n)
	for i := range n {
		gvkps[i] = groupVersionKindPlural{
			GroupVersionKind: schema.GroupVersionKind{
				Group:   fmt.Sprintf("group%d.example.com", i),
				Version: "v1",
				Kind:    fmt.Sprintf("Kind%d", i),
			},
			Plural: fmt.Sprintf("kind%ds", i),
		}
	}
	return gvkps
}

func heapKB() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse / 1024
}

// TestMemoryLeakSimulation runs the buggy and fixed AppendToMap side-by-side
// across many poll cycles and reports heap growth and map entry counts.
func TestMemoryLeakSimulation(t *testing.T) {
	const (
		numGVKs    = 5
		pollCycles = 500
	)

	gvkps := makeGVKPs(numGVKs)

	buggyDiscoverer := &CRDiscoverer{}
	heapBefore := heapKB()
	goroutinesBefore := runtime.NumGoroutine()

	for range pollCycles {
		appendToMapBuggy(buggyDiscoverer, gvkps...)
	}

	heapAfterBuggy := heapKB()
	goroutinesAfterBuggy := runtime.NumGoroutine()

	buggyKindCount := 0
	for _, versions := range buggyDiscoverer.Map {
		for _, kinds := range versions {
			buggyKindCount += len(kinds)
		}
	}
	buggyChannelCount := len(buggyDiscoverer.GVKToReflectorStopChanMap)

	fixedDiscoverer := &CRDiscoverer{}
	heapBeforeFixed := heapKB()

	for range pollCycles {
		fixedDiscoverer.AppendToMap(gvkps...)
	}

	heapAfterFixed := heapKB()

	fixedKindCount := 0
	for _, versions := range fixedDiscoverer.Map {
		for _, kinds := range versions {
			fixedKindCount += len(kinds)
		}
	}
	fixedChannelCount := len(fixedDiscoverer.GVKToReflectorStopChanMap)

	t.Logf("Simulation: %d GVKs × %d poll cycles", numGVKs, pollCycles)
	t.Logf("")
	t.Logf("                       │  BUGGY (pre-fix)  │  FIXED (post-fix)")
	t.Logf("  ─────────────────────┼───────────────────┼──────────────────")
	t.Logf("  Kind entries in map  │  %17d  │  %d", buggyKindCount, fixedKindCount)
	t.Logf("  Stop channels live   │  %17d  │  %d", buggyChannelCount, fixedChannelCount)
	t.Logf("  Heap before (KB)     │  %17d  │  %d", heapBefore, heapBeforeFixed)
	t.Logf("  Heap after  (KB)     │  %17d  │  %d", heapAfterBuggy, heapAfterFixed)
	t.Logf("  Heap growth (KB)     │  %17d  │  %d", int64(heapAfterBuggy)-int64(heapBefore), int64(heapAfterFixed)-int64(heapBeforeFixed)) //nolint:gosec
	t.Logf("  Goroutines before    │  %17d  │  (same baseline)", goroutinesBefore)
	t.Logf("  Goroutines after     │  %17d  │  (no goroutines started)", goroutinesAfterBuggy)

	if buggyKindCount != numGVKs*pollCycles {
		t.Errorf("[buggy] expected %d kind entries (linear growth), got %d", numGVKs*pollCycles, buggyKindCount)
	}
	if fixedKindCount != numGVKs {
		t.Errorf("[fixed] expected exactly %d kind entries (stable), got %d", numGVKs, fixedKindCount)
	}
	if fixedChannelCount != numGVKs {
		t.Errorf("[fixed] expected exactly %d stop channels (stable), got %d", numGVKs, fixedChannelCount)
	}

	buggyGrowth := int64(heapAfterBuggy) - int64(heapBefore) //nolint:gosec
	fixedGrowth := int64(heapAfterFixed) - int64(heapBeforeFixed) //nolint:gosec
	if buggyGrowth > 0 && fixedGrowth >= buggyGrowth {
		t.Errorf("fixed heap growth (%d KB) should be less than buggy growth (%d KB)", fixedGrowth, buggyGrowth)
	}
}

// TestGoroutineLeakSimulation compares goroutine accumulation between the old
// reflector pattern (only GVK stop channel) and the fixed pattern (also listens
// to context cancellation).
func TestGoroutineLeakSimulation(t *testing.T) {
	const (
		numGVKs  = 5
		rebuilds = 20
	)

	goroutinesBefore := runtime.NumGoroutine()

	var leakedChans []chan struct{}
	for range rebuilds {
		for range numGVKs {
			stopCh := make(chan struct{})
			leakedChans = append(leakedChans, stopCh)
			go func(ch chan struct{}) {
				<-ch
			}(stopCh)
		}
	}

	time.Sleep(20 * time.Millisecond)
	goroutinesAfterBuggy := runtime.NumGoroutine()
	buggyLeak := goroutinesAfterBuggy - goroutinesBefore

	for _, ch := range leakedChans {
		close(ch)
	}
	time.Sleep(20 * time.Millisecond)

	goroutinesBeforeFixed := runtime.NumGoroutine()

	for range rebuilds {
		ctx, cancel := context.WithCancel(context.Background())
		for range numGVKs {
			gvkStopCh := make(chan struct{})
			stopCh := make(chan struct{})
			go func() {
				defer close(stopCh)
				select {
				case <-gvkStopCh:
				case <-ctx.Done():
				}
			}()
			_ = stopCh
		}
		cancel()
		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(20 * time.Millisecond)
	goroutinesAfterFixed := runtime.NumGoroutine()
	fixedLeak := goroutinesAfterFixed - goroutinesBeforeFixed

	t.Logf("Goroutine leak simulation: %d GVKs × %d store rebuilds = %d goroutines spawned",
		numGVKs, rebuilds, numGVKs*rebuilds)
	t.Logf("")
	t.Logf("                       │  BUGGY (pre-fix)  │  FIXED (post-fix)")
	t.Logf("  ─────────────────────┼───────────────────┼──────────────────")
	t.Logf("  Goroutines leaked    │  %17d  │  %d", buggyLeak, fixedLeak)
	t.Logf("  Expected leaked      │  %17d  │  0", numGVKs*rebuilds)

	if buggyLeak < numGVKs*rebuilds {
		t.Errorf("[buggy] expected at least %d leaked goroutines, got %d", numGVKs*rebuilds, buggyLeak)
	}
	if fixedLeak > 5 {
		t.Errorf("[fixed] goroutines leaked: %d (expected ~0)", fixedLeak)
	}
}
