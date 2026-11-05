// Package logging sets up log/slog (spec section 8): a console format that
// is coloured on a terminal and plain otherwise, or JSON. Output is
// unbuffered so that docker logs shows it immediately.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// New returns a logger writing to w in the given format ("console" or
// "json") at the given level ("debug", "info", "warn", "error"). If buf is
// not nil, every record is also kept in it for the web UI.
func New(w io.Writer, format, level string, buf *Buffer) *slog.Logger {
	lvl := ParseLevel(level)
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	} else {
		h = &consoleHandler{w: w, level: lvl, color: isTerminal(w), mu: &sync.Mutex{}}
	}
	if buf != nil {
		h = &teeHandler{out: h, buf: &consoleHandler{level: lvl, mu: &sync.Mutex{}, sink: buf}}
	}
	return slog.New(h)
}

// ParseLevel maps a level name to a slog.Level (info if unknown).
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// consoleHandler writes "2006-01-02 15:04:05 INF message key=value ...".
type consoleHandler struct {
	w      io.Writer
	sink   *Buffer // if set, records go here instead of w
	level  slog.Level
	color  bool
	attrs  []slog.Attr
	groups []string
	mu     *sync.Mutex
}

const (
	reset  = "\x1b[0m"
	dim    = "\x1b[2m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	blue   = "\x1b[34m"
	cyan   = "\x1b[36m"
)

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	if h.sink == nil {
		h.paint(&b, dim, ts.Format("2006-01-02 15:04:05"))
		b.WriteByte(' ')
		h.writeLevel(&b, r.Level)
		b.WriteByte(' ')
	}
	b.WriteString(h.colourMessage(r.Message))
	h.writeAttrs(&b, r)
	if h.sink != nil {
		h.sink.add(Entry{Time: ts, Level: levelName(r.Level), Line: b.String()})
		return nil
	}
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *consoleHandler) writeLevel(b *strings.Builder, l slog.Level) {
	switch {
	case l >= slog.LevelError:
		h.paint(b, red, "ERR")
	case l >= slog.LevelWarn:
		h.paint(b, yellow, "WRN")
	case l >= slog.LevelInfo:
		h.paint(b, green, "INF")
	default:
		h.paint(b, blue, "DBG")
	}
}

func (h *consoleHandler) writeAttrs(b *strings.Builder, r slog.Record) {
	prefix := strings.Join(h.groups, ".")
	for _, a := range h.attrs {
		h.writeAttr(b, prefix, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		h.writeAttr(b, prefix, a)
		return true
	})
}

// colourMessage colours sync actions so created/updated/deleted stand out.
func (h *consoleHandler) colourMessage(msg string) string {
	if !h.color {
		return msg
	}
	switch {
	case strings.HasSuffix(msg, "created"):
		return green + msg + reset
	case strings.HasSuffix(msg, "updated"):
		return cyan + msg + reset
	case strings.HasSuffix(msg, "deleted"):
		return red + msg + reset
	}
	return msg
}

func (h *consoleHandler) paint(b *strings.Builder, colour, s string) {
	if h.color {
		b.WriteString(colour)
		b.WriteString(s)
		b.WriteString(reset)
		return
	}
	b.WriteString(s)
}

func (h *consoleHandler) writeAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			h.writeAttr(b, key, ga)
		}
		return
	}
	b.WriteByte(' ')
	h.paint(b, dim, key+"=")
	b.WriteString(quote(a.Value.String()))
}

func quote(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\n\"=") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &n
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.groups = append(append([]string(nil), h.groups...), name)
	return &n
}

// teeHandler sends records to the output handler and to the log buffer.
type teeHandler struct {
	out slog.Handler
	buf slog.Handler
}

func (t *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.out.Enabled(ctx, l) || t.buf.Enabled(ctx, l)
}

func (t *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	if t.buf.Enabled(ctx, r.Level) {
		_ = t.buf.Handle(ctx, r.Clone())
	}
	if t.out.Enabled(ctx, r.Level) {
		return t.out.Handle(ctx, r)
	}
	return nil
}

func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{out: t.out.WithAttrs(attrs), buf: t.buf.WithAttrs(attrs)}
}

func (t *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{out: t.out.WithGroup(name), buf: t.buf.WithGroup(name)}
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}

// Entry is one buffered log line.
type Entry struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	// Line is the message and attributes, without time and level.
	Line string `json:"line"`
}

// Buffer keeps the most recent log lines in memory (spec 7.1).
type Buffer struct {
	mu      sync.Mutex
	entries []Entry
	next    int
	full    bool
}

// NewBuffer returns a buffer that keeps the last size lines.
func NewBuffer(size int) *Buffer {
	return &Buffer{entries: make([]Entry, size)}
}

func (b *Buffer) add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[b.next] = e
	b.next = (b.next + 1) % len(b.entries)
	if b.next == 0 {
		b.full = true
	}
}

// Entries returns the buffered lines at or above minLevel, oldest first.
func (b *Buffer) Entries(minLevel string) []Entry {
	min := ParseLevel(minLevel)
	if minLevel == "" {
		min = slog.LevelDebug
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var ordered []Entry
	if b.full {
		ordered = append(ordered, b.entries[b.next:]...)
	}
	ordered = append(ordered, b.entries[:b.next]...)
	out := make([]Entry, 0, len(ordered))
	for _, e := range ordered {
		if ParseLevel(e.Level) >= min {
			out = append(out, e)
		}
	}
	return out
}

// Clear empties the buffer.
func (b *Buffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.entries {
		b.entries[i] = Entry{}
	}
	b.next, b.full = 0, false
}
