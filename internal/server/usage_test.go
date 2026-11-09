package server

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestUsageJSON(t *testing.T) {
	for _, tc := range []struct {
		path, body, status string
		in, out, total     *int64
	}{
		{"/v1/chat/completions", `{"usage":{"prompt_tokens":2,"completion_tokens":3}}`, "reported", i64(2), i64(3), i64(5)},
		{"/v1/completions", `{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`, "reported", i64(0), i64(0), i64(0)},
		{"/v1/responses", `{"usage":{"input_tokens":4,"output_tokens":5,"total_tokens":12}}`, "reported", i64(4), i64(5), i64(12)},
		{"/v1/messages", `{"usage":{"input_tokens":4}}`, "partial", i64(4), nil, nil},
		{"/v1/embeddings", `{"usage":{"prompt_tokens":8,"total_tokens":8}}`, "reported", i64(8), nil, i64(8)},
		{"/v1/chat/completions", `{"other":{"usage":{"prompt_tokens":4}}}`, "unavailable", nil, nil, nil},
		{"/v1/chat/completions", `{"usage":{"prompt_tokens":1},"usage":{"prompt_tokens":2}}`, "unavailable", nil, nil, nil},
		{"/v1/chat/completions", `{"usage":{"prompt_tokens":-1}}`, "unavailable", nil, nil, nil},
	} {
		u := ParseUsageJSON(tc.path, strings.NewReader(tc.body), "")
		if u.Status != tc.status || !sameInt(u.Input, tc.in) || !sameInt(u.Output, tc.out) || !sameInt(u.Total, tc.total) {
			t.Errorf("%s: got %+v", tc.path, u)
		}
	}
	if u := ParseUsageJSON("/v1/chat/completions", strings.NewReader(`{"usage":{"prompt_tokens":1}}`), "gzip"); u.Status != "unavailable" {
		t.Fatalf("accepted encoded usage: %+v", u)
	}
}

func TestJSONUsageObserverFeed(t *testing.T) {
	var body = []byte(`{"u\u0073age":{"prompt_tokens":0,"completion_tokens":2},"choices":[{"text":"ignored"}]}`)
	o := newJSONUsageObserver("/v1/chat/completions")
	for _, b := range body {
		o.Feed([]byte{b})
	}
	u := o.Finish()
	if u.Status != "reported" || !sameInt(u.Input, i64(0)) || !sameInt(u.Output, i64(2)) || !sameInt(u.Total, i64(2)) {
		t.Fatalf("got %+v", u)
	}
}

func TestJSONUsageObserverBoundsAndMalformedJSON(t *testing.T) {
	deep := strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65)
	validDepth := strings.Repeat("[", 63) + `0` + strings.Repeat("]", 63)
	badUTF8 := `{"skip":"` + string([]byte{0xc3}) + `","usage":{"prompt_tokens":1}}`
	for _, body := range []string{
		`{"usage":{"prompt_tokens":1,}}`,
		`{"usage":{"prompt_tokens":1}} trailing`,
		`{"usage":{"prompt_tokens":1,"prompt_tokens":2}}`,
		`{"usage":{"prompt_tokens":1.0}}`,
		`{"usage":{"prompt_tokens":9223372036854775808}}`,
		`{"usage":{"prompt_tokens":-1}}`,
		`{"usage":{"prompt_tokens":"1"}}`,
		`{"metadata":` + deep + `,"usage":{"prompt_tokens":1}}`,
		badUTF8,
		`{"usage":{"x":"` + strings.Repeat("x", 4096) + `","prompt_tokens":1}}`,
	} {
		o := newJSONUsageObserver("/v1/chat/completions")
		for i := 0; i < len(body); i += 7 {
			end := min(i+7, len(body))
			o.Feed([]byte(body[i:end]))
		}
		if got := o.Finish(); got.Status != "unavailable" {
			t.Errorf("accepted malformed/beyond-bound JSON: %+v body-prefix=%q", got, body[:min(60, len(body))])
		}
	}
	body := `{"usage":{"prompt_tokens":1},"skip":"` + strings.Repeat("x", 100_000) + `"}`
	o := newJSONUsageObserver("/v1/chat/completions")
	for i := 0; i < len(body); i += 113 {
		o.Feed([]byte(body[i:min(i+113, len(body))]))
	}
	if got := o.Finish(); got.Status != "partial" || got.Input == nil || *got.Input != 1 {
		t.Fatalf("large skipped string affected usage: %+v", got)
	}
	if len(o.capture) > 4096 || len(o.stringRaw) > 64 || len(o.stack) > 64 || len(o.number) > 20 {
		t.Fatalf("parser retained state beyond bounds: capture=%d key=%d depth=%d number=%d", len(o.capture), len(o.stringRaw), len(o.stack), len(o.number))
	}
	o = newJSONUsageObserver("/v1/chat/completions")
	o.Feed([]byte(`{"metadata":` + validDepth + `,"usage":{"prompt_tokens":1}}`))
	if got := o.Finish(); got.Status != "partial" {
		t.Fatalf("rejected maximum supported nesting: %+v", got)
	}
	o = newJSONUsageObserver("/v1/chat/completions")
	for _, b := range []byte(`{"skip":"é","usage":{"prompt_tokens":1}}`) {
		o.Feed([]byte{b})
	}
	if got := o.Finish(); got.Status != "partial" {
		t.Fatalf("rejected valid split UTF-8: %+v", got)
	}
	o = newJSONUsageObserver("/v1/chat/completions")
	for _, b := range []byte(badUTF8) {
		o.Feed([]byte{b})
	}
	if got := o.Finish(); got.Status != "unavailable" {
		t.Fatalf("accepted invalid split UTF-8: %+v", got)
	}
}

func TestUsageSSE(t *testing.T) {
	u := ParseUsageSSE("/v1/chat/completions", strings.NewReader("data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\r\n\r\n"))
	if u.Status != "reported" || *u.Input != 1 || *u.Output != 2 {
		t.Fatalf("%+v", u)
	}
}

func TestUsageSSEProviderShapesAndAnthropicCumulativeUsage(t *testing.T) {
	responses := observeSSE("/v1/responses", "", "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":5}}}\r\n\r\n", 1)
	if responses.Status != "reported" || !sameInt(responses.Input, i64(4)) || !sameInt(responses.Output, i64(5)) || !sameInt(responses.Total, i64(9)) {
		t.Fatalf("Responses usage: %+v", responses)
	}
	missingType := observeSSE("/v1/responses", "", "data: {\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":5}}}\n\n", 7)
	if missingType.Status != "unavailable" {
		t.Fatalf("accepted Responses event without type: %+v", missingType)
	}
	completion := observeSSE("/v1/completions", "", "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\n", 2)
	if completion.Status != "reported" || !sameInt(completion.Input, i64(2)) || !sameInt(completion.Output, i64(3)) {
		t.Fatalf("completion usage: %+v", completion)
	}
	anthropic := observeSSE("/v1/messages", "", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\r\rdata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\r\rdata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\r\r", 1)
	if anthropic.Status != "reported" || !sameInt(anthropic.Input, i64(10)) || !sameInt(anthropic.Output, i64(7)) || !sameInt(anthropic.Total, i64(17)) {
		t.Fatalf("Anthropic cumulative usage: %+v", anthropic)
	}
}

func TestUsageSSEFramingBoundsAndIncompleteEvents(t *testing.T) {
	multiline := observeSSE("/v1/chat/completions", "", ": comment\r\nevent: ignored\rdata: {\"usage\":{\"prompt_tokens\":4,\r\ndata: \"completion_tokens\":5}}\r\r", 1)
	if multiline.Status != "reported" || !sameInt(multiline.Input, i64(4)) || !sameInt(multiline.Output, i64(5)) {
		t.Fatalf("multiline SSE: %+v", multiline)
	}
	unfinished := observeSSE("/v1/chat/completions", "", "data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":5}}", 3)
	if unfinished.Status != "unavailable" {
		t.Fatalf("dispatched unfinished event: %+v", unfinished)
	}
	encoded := observeSSE("/v1/chat/completions", "gzip", "data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":5}}\n\n", 17)
	if encoded.Status != "unavailable" {
		t.Fatalf("accepted encoded event: %+v", encoded)
	}
	valid := `{"usage":{"prompt_tokens":1},"x":"` + strings.Repeat("x", 65536-len(`{"usage":{"prompt_tokens":1},"x":""}`)) + `"}`
	if got := observeSSE("/v1/chat/completions", "", "data: "+valid+"\n\n", 1024); got.Status != "partial" {
		t.Fatalf("rejected 64-KiB event: %+v", got)
	}
	tooLarge := `{"usage":{"prompt_tokens":1},"x":"` + strings.Repeat("x", 65537-len(`{"usage":{"prompt_tokens":1},"x":""}`)) + `"}`
	if got := observeSSE("/v1/chat/completions", "", "data: "+tooLarge+"\n\n", 1024); got.Status != "unavailable" {
		t.Fatalf("accepted oversized event: %+v", got)
	}
	sticky := observeSSE("/v1/chat/completions", "", "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\ndata: {bad}\n\ndata: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\n", 4096)
	if sticky.Status != "unavailable" {
		t.Fatalf("accounting recovered after malformed event: %+v", sticky)
	}
	done := observeSSE("/v1/chat/completions", "", "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", 1024)
	if done.Status != "reported" || !sameInt(done.Input, i64(1)) {
		t.Fatalf("DONE marker changed usage: %+v", done)
	}
}

func observeSSE(path, encoding, stream string, chunk int) TokenUsage {
	o := NewUsageObserver(path, encoding)
	for i := 0; i < len(stream); i += chunk {
		o.Feed([]byte(stream[i:min(i+chunk, len(stream))]))
	}
	return o.Result()
}

func TestUsageReadThrough(t *testing.T) {
	r := &testUsageReader{Reader: &scriptedUsageReader{steps: []readStep{{data: []byte("abc")}, {err: io.EOF}}}}
	w := NewUsageReadCloser(r, nil)
	b := make([]byte, 8)
	n, err := w.Read(b)
	if n != 3 || err != nil || string(b[:n]) != "abc" {
		t.Fatalf("%d %v %q", n, err, b[:n])
	}
	if n, err = w.Read(b); n != 0 || err != io.EOF {
		t.Fatalf("second read: %d %v", n, err)
	}
}

func TestUsageReadThroughPreservesBytesErrorsAndClose(t *testing.T) {
	sentinel := errors.New("read failed")
	for _, err := range []error{io.EOF, sentinel} {
		r := &scriptedReadCloser{steps: []readStep{{data: []byte("abc"), err: err}}}
		var observed []byte
		w := NewUsageReadCloser(r, func(p []byte) { observed = append(observed, p...) })
		buf := make([]byte, 8)
		n, gotErr := w.Read(buf)
		if n != 3 || gotErr != err || string(buf[:n]) != "abc" || !bytes.Equal(observed, []byte("abc")) {
			t.Fatalf("read=(%d,%v,%q), observed=%q", n, gotErr, buf[:n], observed)
		}
		if closeErr := w.Close(); closeErr != nil || !r.closed {
			t.Fatalf("close err=%v closed=%v", closeErr, r.closed)
		}
	}
}

type readStep struct {
	data []byte
	err  error
}
type scriptedUsageReader struct{ steps []readStep }

func (r *scriptedUsageReader) Read(p []byte) (int, error) {
	s := r.steps[0]
	r.steps = r.steps[1:]
	return copy(p, s.data), s.err
}

type scriptedReadCloser struct {
	steps  []readStep
	closed bool
}

func (r *scriptedReadCloser) Read(p []byte) (int, error) {
	s := r.steps[0]
	r.steps = r.steps[1:]
	return copy(p, s.data), s.err
}
func (r *scriptedReadCloser) Close() error { r.closed = true; return nil }

func i64(v int64) *int64 { return &v }
func sameInt(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

type testUsageReader struct{ io.Reader }

func (testUsageReader) Close() error { return nil }
