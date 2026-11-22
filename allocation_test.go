package goja

import (
	"errors"
	"testing"
)

var errTestQuota = errors.New("test quota")

// testMeter charges each start once at the most it was charged, without releasing anything, and refuses past limit as a host would: it interrupts the runtime and panics.
type testMeter struct {
	rt      *Runtime
	limit   int64
	used    int64
	held    map[*byte]int64
	charges int
}

func newTestMeter(limit int64) *testMeter {
	m := &testMeter{rt: New(), limit: limit, held: map[*byte]int64{}}
	m.rt.SetAllocationHooks(m.check, m.charge)
	return m
}

func (m *testMeter) check(n int64) {
	if m.used+n > m.limit {
		m.rt.Interrupt(errTestQuota)
		panic(m.rt.NewGoError(errTestQuota))
	}
}

func (m *testMeter) charge(p *byte, n int64) {
	m.charges++
	if had := m.held[p]; n > had {
		m.used += n - had
		m.held[p] = n
	}
}

func TestAllocationAppendsChargeWhatTheyAdd(t *testing.T) {
	m := newTestMeter(64 << 20)
	v, err := m.rt.RunString(`let s = ""; for (let i = 0; i < 12000; i++) s += "x"; s.length`)
	if err != nil {
		t.Fatal(err)
	}
	if v.ToInteger() != 12000 {
		t.Fatalf("length %v, want 12000", v)
	}
	// Each append adds a node and a byte; the full length of every rope in the chain would be about 144 MB.
	if want := 12000 * (ropeNodeSize + 1); m.used > want {
		t.Fatalf("charged %d bytes for 12000 appends, want at most %d", m.used, want)
	}
	// The first appends make flat strings up to concatThreshold, charged one by one.
	if m.charges > 12000*int(ropeNodeSize+1)/ropeChargeMin+concatThreshold+1 {
		t.Fatalf("made %d charges, want them batched by %d bytes", m.charges, ropeChargeMin)
	}
}

func TestAllocationAppendsStopAtTheLimit(t *testing.T) {
	for _, code := range []string{
		`let s = ""; for (;;) s += "x";`,
		`let s = "", c = "x".repeat(1024); for (;;) s += c;`,
		`let s = "x".repeat(64); for (;;) s = s + s;`,
	} {
		m := newTestMeter(1 << 20)
		_, err := m.rt.RunString(`try { ` + code + ` } catch (_) {}`)
		var interrupted *InterruptedError
		if !errors.As(err, &interrupted) || interrupted.Value() != errTestQuota {
			t.Fatalf("%s returned %v, want the quota", code, err)
		}
		if m.used > m.limit {
			t.Fatalf("%s charged %d past the limit %d", code, m.used, m.limit)
		}
	}
}

func TestAllocationReplacementCapturesAreMetered(t *testing.T) {
	for _, code := range []string{
		`RegExp.prototype[Symbol.replace].call({ flags: "", exec() { return { 0: "x", index: 0, length: 4096 }; } }, "x", "y")`,
		`RegExp.prototype[Symbol.replace].call({ flags: "", exec() { return { 0: "x", index: 0, length: 4096 }; } }, "x", () => "y")`,
		// Adding named groups grows an already-metered callback argument list.
		`RegExp.prototype[Symbol.replace].call({ flags: "", exec() { return { 0: "x", index: 0, length: 512, groups: {} }; } }, "x", () => "y")`,
		`RegExp.prototype[Symbol.replace].call({ flags: "", exec() { return { 0: "x", index: 0, length: 2 ** 32 }; } }, "x", "y")`,
	} {
		m := newTestMeter(16 << 10)
		_, err := m.rt.RunString(`try { ` + code + ` } catch (_) {}`)
		var interrupted *InterruptedError
		if !errors.As(err, &interrupted) || interrupted.Value() != errTestQuota {
			t.Errorf("%s returned %v, want the quota", code, err)
		}
		if m.used > m.limit {
			t.Errorf("%s charged %d past the limit %d", code, m.used, m.limit)
		}
	}
}

func TestAllocationReplacementCapturesPreserveArguments(t *testing.T) {
	m := newTestMeter(16 << 10)
	value, err := m.rt.RunString(`let seen;
		let result = RegExp.prototype[Symbol.replace].call({
			flags: "", exec() { return { 0: "b", 1: "a", index: 1, length: 2, groups: { word: "a" } }; }
		}, "aba", function (match, capture, position, input, groups) {
			seen = [match, capture, position, input, groups.word].join(":");
			return "<" + capture + ">";
		});
		result + "|" + seen`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "a<a>a|b:a:1:aba:a"; value.String() != want {
		t.Fatalf("replacement = %s, want %s", value, want)
	}
}

func TestAllocationFlattenChargesOnce(t *testing.T) {
	m := newTestMeter(64 << 20)
	if _, err := m.rt.RunString(`var s = ""; for (let i = 0; i < 2000; i++) s += "xy";`); err != nil {
		t.Fatal(err)
	}
	before := m.used
	v, err := m.rt.RunString(`let n = 0; for (let i = 0; i < 100; i++) n += s.length + s.charCodeAt(i) + (s + "z").length + s.indexOf("y"); n`)
	if err != nil {
		t.Fatal(err)
	}
	if v.ToInteger() == 0 {
		t.Fatal("reads returned nothing")
	}
	// One flat copy of s, plus 100 small ropes appending "z" to it.
	if want := before + 4000 + 100*(ropeNodeSize+1); m.used > want {
		t.Fatalf("charged %d bytes after reading the rope, want at most %d", m.used, want)
	}
}

func TestAllocationRefusedFlattenKeepsTheRope(t *testing.T) {
	m := newTestMeter(64 << 20)
	v, err := m.rt.RunString(`let s = ""; for (let i = 0; i < 2000; i++) s += "xy"; s`)
	if err != nil {
		t.Fatal(err)
	}
	rope, ok := v.(*concatString)
	if !ok {
		t.Fatalf("built %T, want a rope", v)
	}
	m.limit = m.used
	// Host code reads the rope: the refusal does not panic here, the flat copy is not kept, and the runtime stops at its next check.
	if got := v.String(); len(got) != 4000 {
		t.Fatalf("read %d bytes, want 4000", len(got))
	}
	if _, right := rope.load(); right == nil {
		t.Fatal("kept the flat copy of a refused rope")
	}
	_, err = m.rt.RunString(`1`)
	var interrupted *InterruptedError
	if !errors.As(err, &interrupted) || interrupted.Value() != errTestQuota {
		t.Fatalf("the next run returned %v, want the quota", err)
	}
}
