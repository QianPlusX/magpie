package gateway

import (
	"testing"
	"time"
)

// restForSeed puts one rest in the gateway's memory table for a test, and
// takes it out again after.
func restForSeed(t *testing.T, id string, until time.Time) {
	t.Helper()
	restingUntil.Lock()
	restingUntil.m[id] = until
	restingUntil.Unlock()
	t.Cleanup(func() {
		restingUntil.Lock()
		delete(restingUntil.m, id)
		delete(restingUntil.note, id)
		restingUntil.Unlock()
	})
}

func TestRestingForCountsAnAccountsAllowance(t *testing.T) {
	now := time.Now()
	until := now.Add(5 * time.Hour)
	restForSeed(t, "probeco@acct", until)
	if got, ok := RestingFor("probeco", "qwen3.8-flash-max", now); !ok || !got.Equal(until) {
		t.Fatalf("an account out of allowance rests every model of it: got %v %v", got, ok)
	}
	if _, ok := RestingFor("probec", "qwen3.8-flash-max", now); ok {
		t.Fatal("probec must not inherit probeco's rest")
	}
	if _, ok := RestingFor("probeco2", "qwen3.8-flash-max", now); ok {
		t.Fatal("probeco2 must not inherit probeco's rest")
	}
}

func TestRestingForKeepsOneModelOfAKey(t *testing.T) {
	now := time.Now()
	until := now.Add(time.Hour)
	restForSeed(t, "probekey#k1/reflection/beam-free", until)
	if got, ok := RestingFor("probekey", "reflection/beam-free", now); !ok || !got.Equal(until) {
		t.Fatalf("a key resting for one model: got %v %v", got, ok)
	}
	if _, ok := RestingFor("probekey", "other-model", now); ok {
		t.Fatal("the key's other models do not rest with it")
	}
}

func TestRestingForForgetsAnEndedRest(t *testing.T) {
	now := time.Now()
	restForSeed(t, "probeended", now.Add(-time.Minute))
	if _, ok := RestingFor("probeended", "m", now); ok {
		t.Fatal("a rest that ended is not a rest")
	}
}
