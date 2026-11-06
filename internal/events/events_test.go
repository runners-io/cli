package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

var fixedClock = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }

func decodeLines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line is not JSON: %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestJSONEmitterWritesOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	e := NewJSON(&buf, fixedClock)
	e.Emit(Event{Name: "syncing", Fields: Fields{"files": 3, "bytes": 120}})
	e.Emit(Event{Name: "exit", Fields: Fields{"code": 7}})

	got := decodeLines(t, buf.Bytes())
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(got), buf.String())
	}
	if got[0]["event"] != "syncing" || got[0]["v"] != float64(SchemaVersion) || got[0]["ts"] != "2026-10-31T12:00:00Z" {
		t.Errorf("envelope wrong: %v", got[0])
	}
	if got[0]["files"] != float64(3) || got[1]["code"] != float64(7) {
		t.Errorf("fields lost: %v %v", got[0], got[1])
	}
	if strings.Count(buf.String(), "\n") != 2 {
		t.Errorf("each event must be exactly one line: %q", buf.String())
	}
}

// A field must never be able to overwrite the envelope, or a consumer that
// switches on "event" could be steered by data.
func TestFieldsCannotOverwriteTheEnvelope(t *testing.T) {
	var buf bytes.Buffer
	NewJSON(&buf, fixedClock).Emit(Event{Name: "stdout", Fields: Fields{"event": "exit", "v": 99, "ts": "x", "data": "hi"}})
	got := decodeLines(t, buf.Bytes())[0]
	if got["event"] != "stdout" || got["v"] != float64(SchemaVersion) || got["ts"] != "2026-10-31T12:00:00Z" {
		t.Fatalf("envelope was overwritten: %v", got)
	}
	if got["data"] != "hi" {
		t.Fatalf("data lost: %v", got)
	}
}

func TestStreamWriterEmitsOneEventPerWriteAndKeepsNewlinesInData(t *testing.T) {
	var buf bytes.Buffer
	e := NewJSON(&buf, fixedClock)
	w := e.Stream("stdout")
	if n, err := w.Write([]byte("line one\nline two\n")); err != nil || n != 18 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	got := decodeLines(t, buf.Bytes())
	if len(got) != 1 || got[0]["event"] != "stdout" || got[0]["data"] != "line one\nline two\n" {
		t.Fatalf("got %v", got)
	}
	// The JSON line itself must still be a single line.
	if strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("data newlines leaked into the framing: %q", buf.String())
	}
}

func TestJSONEmitterIsSafeForConcurrentUse(t *testing.T) {
	var buf bytes.Buffer
	e := NewJSON(&buf, fixedClock)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Stream("stdout").Write([]byte("a"))
			e.Stream("stderr").Write([]byte("b"))
		}()
	}
	wg.Wait()
	if got := decodeLines(t, buf.Bytes()); len(got) != 80 {
		t.Fatalf("got %d intact lines, want 80 (interleaved writes?)", len(got))
	}
}

func TestHumanEmitterRendersProgressAndDropsStreams(t *testing.T) {
	var buf bytes.Buffer
	h := NewHuman(&buf)
	h.Emit(Event{Name: "syncing", Fields: Fields{"files": 12, "bytes": 2048, "skipped": 2}})
	h.Emit(Event{Name: "skipped", Fields: Fields{"path": ".env", "reason": "secret"}})
	h.Emit(Event{Name: "billed", Fields: Fields{"seconds": 90}})
	h.Emit(Event{Name: "error", Fields: Fields{"kind": "auth", "message": "no credential"}})
	h.Emit(Event{Name: "warning", Fields: Fields{"message": "careful"}})
	// A stream event is raw program output; the human emitter must not print
	// it a second time (the CLI passes the bytes straight through).
	h.Emit(Event{Name: "stdout", Fields: Fields{"data": "PROGRAM OUTPUT"}})

	out := buf.String()
	for _, want := range []string{"syncing 12 files", "skipped .env (secret)", "billed 90s", "error: no credential", "warning: careful"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PROGRAM OUTPUT") {
		t.Errorf("stream data was rendered by the human emitter:\n%s", out)
	}
}

func TestDiscardDoesNothing(t *testing.T) {
	Discard.Emit(Event{Name: "x"})
}
