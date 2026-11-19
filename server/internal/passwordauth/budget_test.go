package passwordauth

import (
	"errors"
	"strconv"
	"sync"
	"testing"
)

func TestNewHasherWithBudgetRejectsParamsOverBudget(t *testing.T) {
	p := cheapParams
	p.MemoryKiB = 128
	if _, err := NewHasherWithBudget(p, 64); err == nil {
		t.Fatal("parameters larger than the budget accepted")
	}
	if _, err := NewHasherWithBudget(p, 128); err != nil {
		t.Fatalf("parameters equal to the budget: %v", err)
	}
}

func TestHasherFromEnvBudget(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}
	h, err := HasherFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if h.budgetKiB != DefaultMemoryBudgetKiB || h.Params() != DefaultParams {
		t.Fatalf("defaults: budget=%d params=%+v", h.budgetKiB, h.Params())
	}
	h, err = HasherFromEnv(env(map[string]string{EnvMemoryBudgetKiB: "131072"}))
	if err != nil {
		t.Fatal(err)
	}
	if h.budgetKiB != 131072 {
		t.Fatalf("explicit budget = %d", h.budgetKiB)
	}
	for _, bad := range []map[string]string{
		{EnvMemoryBudgetKiB: "lots"},
		{EnvMemoryBudgetKiB: "1024"},
		{EnvMemoryBudgetKiB: strconv.Itoa(maxMemoryBudgetKiB + 1)},
		// The default 64 MiB parameters do not fit a 32 MiB budget.
		{EnvMemoryBudgetKiB: "32768"},
		// Nor do 512 MiB parameters fit the default budget.
		{EnvMemoryKiB: "524288"},
	} {
		if _, err := HasherFromEnv(env(bad)); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// After the parameters are lowered, hashes made with the old ones still
// reserve their own, larger memory, so the budget holds either way.
func TestBudgetCountsStoredHashParams(t *testing.T) {
	old := cheapParams
	old.MemoryKiB, old.Iterations = 128, 32
	oldHasher := newTestHasher(t, old)
	oldHash, err := oldHasher.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}

	current := cheapParams
	current.Iterations = 32
	h, err := NewHasherWithBudget(current, 256)
	if err != nil {
		t.Fatal(err)
	}
	h.peakKiB.Store(0)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if ok, err := h.Verify(testPassword, oldHash); !ok || err != nil {
					t.Errorf("verify old hash: ok=%v err=%v", ok, err)
				}
			} else {
				h.VerifyDummy(testPassword)
			}
		}(i)
	}
	wg.Wait()
	if peak := h.peakKiB.Load(); peak == 0 || peak > 256 {
		t.Fatalf("peak reserved memory %d KiB, budget 256", peak)
	}
	if h.inUseKiB.Load() != 0 {
		t.Fatalf("memory still reserved: %d", h.inUseKiB.Load())
	}

	// A stored hash larger than the whole budget is refused, not run.
	small, err := NewHasherWithBudget(current, 64)
	if err != nil {
		t.Fatal(err)
	}
	small.peakKiB.Store(0)
	if ok, err := small.Verify(testPassword, oldHash); ok || !errors.Is(err, ErrHashOverBudget) {
		t.Fatalf("over-budget hash: ok=%v err=%v", ok, err)
	}
	if small.peakKiB.Load() != 0 {
		t.Fatal("over-budget hash was computed")
	}
	if !small.NeedsRehash(oldHash) {
		t.Fatal("old hash does not need a rehash")
	}
}
