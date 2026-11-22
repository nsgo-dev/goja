package goja

import (
	"errors"
	"slices"
	"testing"
)

// testJobContext assigns a distinct context to each group of reactions.
type testJobContext struct {
	current string
}

func (c *testJobContext) Grab() any       { return c.current }
func (c *testJobContext) Resumed(ctx any) { c.current, _ = ctx.(string) }
func (c *testJobContext) Exited()         { c.current = "" }

func TestRetainUnstartedPromiseJobs(t *testing.T) {
	for _, retain := range []bool{false, true} {
		name := "default"
		if retain {
			name = "retain"
		}
		t.Run(name, func(t *testing.T) {
			rt := New()
			ctx := &testJobContext{}
			rt.SetAsyncContextTracker(ctx)
			cancelled := false
			if retain {
				rt.SetPromiseJobFilter(func(owner any) bool { return !cancelled || owner != "cancelled" })
			}
			var records []string
			rt.Set("record", func(name string) { records = append(records, name) })
			rt.Set("cancel", func() { rt.Interrupt("cancel the test job") })
			promise, resolve, _ := rt.NewPromise()
			rt.Set("start", promise)
			queue := func(owner, script string) {
				t.Helper()
				ctx.current = owner
				if _, err := rt.RunString(script); err != nil {
					t.Fatal(err)
				}
			}
			queue("first", `start.then(() => { record("first"); Promise.resolve().then(() => record("next batch")) })`)
			queue("cancelled", `
				start.then(() => {
					Promise.resolve({then() { record("discarded thenable") }});
					cancel();
					record("cancelled job completed");
				});
				start.then(() => record("discarded"));
			`)
			queue("sibling", `start.then(() => record("sibling"))`)
			var interrupt *InterruptedError
			if err := resolve(nil); !errors.As(err, &interrupt) {
				t.Fatalf("settling the promise returned %v, want InterruptedError", err)
			}
			cancelled = true
			ctx.current = ""
			if err := rt.DrainPromiseJobs(); err != nil {
				t.Fatal(err)
			}
			want := []string{"first"}
			if retain {
				want = append(want, "sibling", "next batch")
			}
			if !slices.Equal(records, want) {
				t.Fatalf("promise jobs ran %v, want %v", records, want)
			}
		})
	}
}

func TestInterruptingPromiseJobRetainsCallError(t *testing.T) {
	const dispatch = `Promise.resolve().then(() => { cancel(); return "unreached"; }); throw new Error("dispatch refused")`
	for _, name := range []string{"script", "call"} {
		t.Run(name, func(t *testing.T) {
			rt := New()
			rt.SetPromiseJobFilter(func(any) bool { return true })
			cut := errors.New("cancel the test job")
			rt.Set("cancel", func() { rt.Interrupt(cut) })
			var err error
			if name == "script" {
				_, err = rt.RunString(dispatch)
			} else {
				var fn Value
				fn, err = rt.RunString("(function () { " + dispatch + " })")
				if err != nil {
					t.Fatal(err)
				}
				call, _ := AssertFunction(fn)
				_, err = call(Undefined())
			}
			var interrupted *InterruptedError
			if !errors.Is(err, cut) || !errors.As(err, &interrupted) {
				t.Fatalf("call returned %v, want the interruption", err)
			}
			var exception *Exception
			if !errors.As(interrupted.CallError(), &exception) {
				t.Fatalf("original call error = %v, want an exception", interrupted.CallError())
			}
			if got := exception.Value().(*Object).Get("message").String(); got != "dispatch refused" {
				t.Fatalf("original call error = %q, want dispatch refused", got)
			}
		})
	}
}
