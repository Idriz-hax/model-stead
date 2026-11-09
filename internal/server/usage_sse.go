package server

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
)

const maxSSEEventBytes = 64 * 1024

type usageObserver struct {
	path      string
	line      []byte
	event     []byte
	data      bool
	pendingCR bool
	result    TokenUsage
	hasUsage  bool
	disabled  bool
}

func NewUsageObserver(path, encoding string) *usageObserver {
	o := &usageObserver{path: path, result: unavailable()}
	if (encoding != "" && encoding != "identity") || path == "/v1/embeddings" || usageKeys(path) == nil {
		o.disabled = true
	}
	return o
}

func (o *usageObserver) Feed(p []byte) {
	if o.disabled {
		return
	}
	for _, b := range p {
		if o.pendingCR {
			o.lineDone()
			o.pendingCR = false
			if b == '\n' {
				continue
			}
		}
		if b == '\r' {
			o.pendingCR = true
			continue
		}
		if b == '\n' {
			o.lineDone()
			continue
		}
		// The framing prefix can add at most six bytes beyond the event budget.
		if len(o.line) >= maxSSEEventBytes+6 {
			o.fail()
			return
		}
		o.line = append(o.line, b)
	}
}

func (o *usageObserver) lineDone() {
	line := o.line
	o.line = o.line[:0]
	if len(line) == 0 {
		o.dispatch()
		return
	}
	if line[0] == ':' {
		return
	}
	field, value, hasColon := bytes.Cut(line, []byte{':'})
	if !hasColon {
		value = nil
	}
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	if !bytes.Equal(field, []byte("data")) {
		return
	}
	sep := 0
	if o.data {
		sep = 1
	}
	if len(o.event)+sep+len(value) > maxSSEEventBytes {
		o.fail()
		return
	}
	if sep == 1 {
		o.event = append(o.event, '\n')
	}
	o.event = append(o.event, value...)
	o.data = true
}

func (o *usageObserver) dispatch() {
	if !o.data {
		return
	}
	event := o.event
	o.event = nil
	o.data = false
	if bytes.Equal(bytes.TrimSpace(event), []byte("[DONE]")) {
		return
	}
	u, eventType, found, invalid := parseSSEUsageEvent(o.path, event)
	if invalid {
		o.fail()
		return
	}
	if !found {
		return
	}
	if o.path == "/v1/messages" && eventType == "message_delta" {
		o.mergeAnthropicDelta(u)
		return
	}
	o.result = u
	o.hasUsage = true
}

func (o *usageObserver) mergeAnthropicDelta(next TokenUsage) {
	if !o.hasUsage {
		o.result = next
		o.hasUsage = true
		return
	}
	if next.Input != nil {
		o.result.Input = next.Input
	}
	if next.Output != nil {
		o.result.Output = next.Output
	}
	o.result.Total = nil
	if o.result.Input != nil && o.result.Output != nil && *o.result.Input <= math.MaxInt64-*o.result.Output {
		total := *o.result.Input + *o.result.Output
		o.result.Total = &total
	}
	if o.result.Input != nil && o.result.Output != nil {
		o.result.Status = "reported"
	} else {
		o.result.Status = "partial"
	}
}

func (o *usageObserver) fail() {
	o.disabled = true
	o.result = unavailable()
	o.hasUsage = false
	o.event = nil
	o.line = nil
}

// Result processes a trailing CR delimiter, but deliberately drops an
// unterminated line or event: only complete SSE events can report usage.
func (o *usageObserver) Result() TokenUsage {
	if o.disabled {
		return unavailable()
	}
	if o.pendingCR {
		o.lineDone()
		o.pendingCR = false
	}
	return o.result
}

func ParseUsageSSE(path string, r io.Reader) TokenUsage {
	o := NewUsageObserver(path, "")
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			o.Feed(buf[:n])
		}
		if err != nil {
			if err != io.EOF {
				o.fail()
			}
			break
		}
		if n == 0 {
			o.fail()
			break
		}
	}
	return o.Result()
}

func parseSSEUsageEvent(path string, event []byte) (TokenUsage, string, bool, bool) {
	dec := json.NewDecoder(bytes.NewReader(event))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return unavailable(), "", false, true
	}
	var eventType string
	typeSeen, usageSeen, responseSeen, messageSeen := false, false, false, false
	var rootUsage, responseUsage, messageUsage []byte
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return unavailable(), "", false, true
		}
		key, ok := keyToken.(string)
		if !ok {
			return unavailable(), "", false, true
		}
		switch key {
		case "type":
			if typeSeen {
				return unavailable(), "", false, true
			}
			typeSeen = true
			t, err := dec.Token()
			if err != nil {
				return unavailable(), "", false, true
			}
			var valid bool
			eventType, valid = t.(string)
			if !valid {
				return unavailable(), "", false, true
			}
		case "usage":
			if path != "/v1/chat/completions" && path != "/v1/completions" && path != "/v1/messages" {
				if err := skipSSEValue(dec); err != nil {
					return unavailable(), "", false, true
				}
				continue
			}
			if usageSeen {
				return unavailable(), "", false, true
			}
			usageSeen = true
			var valid bool
			rootUsage, valid = captureSSEObject(dec, event)
			if !valid {
				return unavailable(), "", false, true
			}
		case "response":
			if path != "/v1/responses" {
				if err := skipSSEValue(dec); err != nil {
					return unavailable(), "", false, true
				}
				continue
			}
			if responseSeen {
				return unavailable(), "", false, true
			}
			responseSeen = true
			var valid bool
			responseUsage, valid = captureNestedSSEUsage(dec, event)
			if !valid {
				return unavailable(), "", false, true
			}
		case "message":
			if path != "/v1/messages" {
				if err := skipSSEValue(dec); err != nil {
					return unavailable(), "", false, true
				}
				continue
			}
			if messageSeen {
				return unavailable(), "", false, true
			}
			messageSeen = true
			var valid bool
			messageUsage, valid = captureNestedSSEUsage(dec, event)
			if !valid {
				return unavailable(), "", false, true
			}
		default:
			if err := skipSSEValue(dec); err != nil {
				return unavailable(), "", false, true
			}
		}
	}
	if tok, err = dec.Token(); err != nil || tok != json.Delim('}') {
		return unavailable(), "", false, true
	}
	if _, err = dec.Token(); err != io.EOF {
		return unavailable(), "", false, true
	}
	var raw []byte
	switch path {
	case "/v1/chat/completions", "/v1/completions":
		raw = rootUsage
	case "/v1/responses":
		if eventType != "response.completed" {
			return unavailable(), eventType, false, false
		}
		raw = responseUsage
	case "/v1/messages":
		switch eventType {
		case "message_start":
			raw = messageUsage
		case "message_delta":
			raw = rootUsage
		default:
			return unavailable(), eventType, false, false
		}
	}
	if raw == nil {
		return unavailable(), eventType, false, false
	}
	u := decodeUsageCapture(path, raw)
	if u.Status == "unavailable" {
		return unavailable(), eventType, false, true
	}
	return u, eventType, true, false
}

func captureSSEObject(dec *json.Decoder, event []byte) ([]byte, bool) {
	start := int(dec.InputOffset())
	for start < len(event) && (event[start] == ':' || event[start] == ' ' || event[start] == '\t' || event[start] == '\r' || event[start] == '\n') {
		start++
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		if err := skipSSEToken(dec, tok); err != nil {
			return nil, false
		}
		return nil, false
	}
	if err := skipSSEToken(dec, tok); err != nil {
		return nil, false
	}
	end := int(dec.InputOffset())
	raw := bytes.TrimSpace(event[start:end])
	return raw, len(raw) > 0 && len(raw) <= 4096
}

func captureNestedSSEUsage(dec *json.Decoder, event []byte) ([]byte, bool) {
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		if err := skipSSEToken(dec, tok); err != nil {
			return nil, false
		}
		return nil, false
	}
	var usage []byte
	seen := false
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false
		}
		if key != "usage" {
			if err := skipSSEValue(dec); err != nil {
				return nil, false
			}
			continue
		}
		if seen {
			return nil, false
		}
		seen = true
		var valid bool
		usage, valid = captureSSEObject(dec, event)
		if !valid {
			return nil, false
		}
	}
	close, err := dec.Token()
	if err != nil || close != json.Delim('}') {
		return nil, false
	}
	return usage, true
}

func skipSSEValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return skipSSEToken(dec, tok)
}

func skipSSEToken(dec *json.Decoder, tok any) error {
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return err
			}
			if err := skipSSEValue(dec); err != nil {
				return err
			}
		}
		close, err := dec.Token()
		if err != nil || close != json.Delim('}') {
			return io.ErrUnexpectedEOF
		}
	case '[':
		for dec.More() {
			if err := skipSSEValue(dec); err != nil {
				return err
			}
		}
		close, err := dec.Token()
		if err != nil || close != json.Delim(']') {
			return io.ErrUnexpectedEOF
		}
	default:
		return io.ErrUnexpectedEOF
	}
	return nil
}
