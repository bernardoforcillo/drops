package drops_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops"
)

func TestCallHookNilIsNoop(t *testing.T) {
	// Must not panic.
	drops.CallHook(nil, context.Background(), drops.QueryEvent{})
}

func TestCallHookRecoversFromPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a panicking hook leaked: %v", r)
		}
	}()
	panicHook := func(context.Context, drops.QueryEvent) {
		panic("user code is buggy")
	}
	drops.CallHook(panicHook, context.Background(), drops.QueryEvent{})
}

func TestChainHooksContinuesAfterPanic(t *testing.T) {
	var ran []string
	a := func(context.Context, drops.QueryEvent) { ran = append(ran, "a") }
	b := func(context.Context, drops.QueryEvent) { panic("boom") }
	c := func(context.Context, drops.QueryEvent) { ran = append(ran, "c") }
	chained := drops.ChainHooks(a, b, c)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("chain leaked panic: %v", r)
		}
	}()
	chained(context.Background(), drops.QueryEvent{Kind: "test"})
	if len(ran) != 2 || ran[0] != "a" || ran[1] != "c" {
		t.Errorf("expected [a c], got %v", ran)
	}
}

func TestCallHookForwardsEventVerbatim(t *testing.T) {
	want := drops.QueryEvent{Kind: "exec", SQL: "SELECT 1"}
	var got drops.QueryEvent
	drops.CallHook(func(_ context.Context, e drops.QueryEvent) {
		got = e
	}, context.Background(), want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
