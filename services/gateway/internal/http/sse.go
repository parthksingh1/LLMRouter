package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

// ErrNoFlusher means the connection cannot stream, so SSE is impossible.
var ErrNoFlusher = errors.New("the response writer does not support flushing")

// SSEWriter emits Server-Sent Events in the exact shape OpenAI clients parse.
//
// Two details matter for compatibility and are easy to get wrong:
//
//   - every frame must be flushed immediately, or a buffering proxy will hold the whole stream
//     and the caller sees no tokens until the end;
//   - the stream must terminate with `data: [DONE]`, which is not valid JSON and which every
//     OpenAI SDK looks for as the end marker.
//
// The writer also supports named events (`event: failover`). Named events are ignored by the
// OpenAI SDKs, which read only `data:` frames, so they can carry gateway metadata without
// breaking a vanilla client.
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	opened  bool
}

// NewSSEWriter wraps a response writer.
func NewSSEWriter(w http.ResponseWriter) *SSEWriter {
	s := &SSEWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		s.flusher = f
	}
	return s
}

// Open writes the SSE response headers. It must be called before any frame.
func (s *SSEWriter) Open() error {
	if s.flusher == nil {
		return ErrNoFlusher
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Tells nginx not to buffer the response; without it a default reverse proxy silently
	// defeats streaming.
	h.Set("X-Accel-Buffering", "no")

	s.w.WriteHeader(http.StatusOK)
	s.flusher.Flush()
	s.opened = true
	return nil
}

// WriteChunk writes one OpenAI-compatible `data:` frame.
func (s *SSEWriter) WriteChunk(chunk openaiapi.ChatCompletionChunk) error {
	payload, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("encoding stream chunk: %w", err)
	}
	return s.writeFrame("", payload)
}

// WriteEvent writes a named event carrying arbitrary JSON.
//
// This is how failover metadata reaches clients that want it. A vanilla OpenAI SDK never sees
// it, because those clients only parse frames with no event name.
func (s *SSEWriter) WriteEvent(name string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding %s event: %w", name, err)
	}
	return s.writeFrame(name, payload)
}

// WriteError delivers an error inside an already-open stream.
//
// Once headers are sent an HTTP status can no longer be changed, so a mid-stream failure has to
// be reported in-band. OpenAI's own API does exactly this, and SDKs surface it.
func (s *SSEWriter) WriteError(message, typ, code string) error {
	return s.writeFrameRaw("", mustJSON(openaiapi.NewError(message, typ, code, "")))
}

// Done writes the terminating [DONE] marker.
func (s *SSEWriter) Done() error {
	return s.writeFrameRaw("", []byte("[DONE]"))
}

// Comment writes an SSE comment, which is a valid keep-alive that every client ignores.
func (s *SSEWriter) Comment(text string) error {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return fmt.Errorf("writing keep-alive: %w", err)
	}
	s.flusher.Flush()
	return nil
}

func (s *SSEWriter) writeFrame(event string, payload []byte) error {
	return s.writeFrameRaw(event, payload)
}

func (s *SSEWriter) writeFrameRaw(event string, payload []byte) error {
	if !s.opened {
		return errors.New("sse writer used before Open")
	}
	if event != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", event); err != nil {
			return fmt.Errorf("writing event name: %w", err)
		}
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		// A write failure here almost always means the client disconnected. Callers stop
		// pumping rather than treating it as a server error.
		return fmt.Errorf("writing stream frame: %w", err)
	}
	s.flusher.Flush()
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// The only values passed here are gateway-owned structs; a failure would be a
		// programming error, and an empty object is still a parseable frame.
		return []byte("{}")
	}
	return b
}
