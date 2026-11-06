// Package events is the CLI's two ways of saying what happened: a human line on
// stderr, or one JSON object per line (NDJSON) on stdout for a script or a
// coding agent.
//
// The JSON form is a contract, so every line carries a schema version (`v`), the
// event name (`event`) and a timestamp (`ts`, RFC 3339 UTC, from an injected
// clock so tests are exact). The fields of an event can never overwrite those
// three. The event names and fields are listed in the README.
package events

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// SchemaVersion is bumped when an event's meaning changes incompatibly.
const SchemaVersion = 1

// Fields are an event's payload.
type Fields map[string]any

// Event is one thing that happened.
type Event struct {
	Name   string
	Fields Fields
}

// Emitter receives events.
type Emitter interface {
	Emit(Event)
}

type discard struct{}

func (discard) Emit(Event) {}

// Discard drops every event.
var Discard Emitter = discard{}

// JSON writes NDJSON to one writer. It is safe for concurrent use: a program's
// stdout and stderr can arrive on different goroutines, and one line must never
// be torn by another.
type JSON struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// NewJSON returns a JSON emitter. now supplies the timestamps.
func NewJSON(w io.Writer, now func() time.Time) *JSON {
	return &JSON{w: w, now: now}
}

// Emit writes one line. Reserved keys in the fields are dropped.
func (j *JSON) Emit(e Event) {
	m := make(map[string]any, len(e.Fields)+3)
	for k, v := range e.Fields {
		m[k] = v
	}
	m["v"] = SchemaVersion
	m["event"] = e.Name
	m["ts"] = j.now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(m)
	if err != nil {
		// A field that cannot be encoded is a bug in the caller. Say so in the
		// stream rather than lose the event silently.
		b, _ = json.Marshal(map[string]any{
			"v": SchemaVersion, "event": "error", "ts": m["ts"],
			"kind": "usage", "message": "internal: event " + e.Name + " could not be encoded",
		})
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	_, _ = j.w.Write(append(b, '\n'))
}

// Stream returns a writer that turns each Write into one event named name with
// the bytes in `data`. Bytes that are not valid UTF-8 are replaced by the JSON
// encoder, so a program that writes binary to stdout loses it under --json; the
// human mode passes the bytes through untouched.
func (j *JSON) Stream(name string) io.Writer {
	return streamWriter{j: j, name: name}
}

type streamWriter struct {
	j    *JSON
	name string
}

func (s streamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.j.Emit(Event{Name: s.name, Fields: Fields{"data": string(p)}})
	return len(p), nil
}

// Human renders progress for a person. It never prints stream events: in the
// human mode a program's output is passed straight through, and printing it a
// second time here would duplicate it.
type Human struct {
	mu sync.Mutex
	w  io.Writer
}

// NewHuman returns a human emitter writing to w (the CLI uses stderr).
func NewHuman(w io.Writer) *Human { return &Human{w: w} }

// Emit renders one event as one line.
func (h *Human) Emit(e Event) {
	var line string
	switch e.Name {
	case "stdout", "stderr":
		return
	case "syncing":
		line = fmt.Sprintf("syncing %v files (%s)", e.Fields["files"], humanBytes(e.Fields["bytes"]))
		if n, ok := e.Fields["skipped"]; ok && fmt.Sprint(n) != "0" {
			line += fmt.Sprintf(", %v skipped", n)
		}
	case "skipped":
		line = fmt.Sprintf("skipped %v (%v)", e.Fields["path"], e.Fields["reason"])
	case "login":
		line = fmt.Sprintf("To sign in, open %v and enter the code %v (it expires in %v)", e.Fields["url"], e.Fields["user_code"], e.Fields["expires_in"])
	case "billed":
		line = fmt.Sprintf("billed %vs", e.Fields["seconds"])
	case "error":
		line = fmt.Sprintf("error: %v", e.Fields["message"])
	case "warning":
		line = fmt.Sprintf("warning: %v", e.Fields["message"])
	case "exit":
		line = fmt.Sprintf("exit %v", e.Fields["code"])
	default:
		keys := make([]string, 0, len(e.Fields))
		for k := range e.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := []string{e.Name}
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, e.Fields[k]))
		}
		line = strings.Join(parts, " ")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, _ = fmt.Fprintln(h.w, line)
}

func humanBytes(v any) string {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int64:
		n = x
	default:
		return fmt.Sprint(v)
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
