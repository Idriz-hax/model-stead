package server

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
)

const (
	jsonObject  byte = '{'
	jsonArray   byte = '['
	objKeyOrEnd      = iota
	objKey
	objColon
	objValue
	objCommaEnd
	arrValueOrEnd
	arrValue
	arrCommaEnd
)

type jsonUsageFrame struct {
	kind       byte
	state      int
	root       bool
	usage      bool
	wantsUsage bool
}

type jsonUsageObserver struct {
	path       string
	stack      []jsonUsageFrame
	rootSeen   bool
	rootDone   bool
	usageSeen  bool
	failed     bool
	lex        byte
	stringKey  bool
	stringRaw  []byte
	stringLong bool
	escape     bool
	unicode    int
	utf8Need   int
	utf8Min    byte
	utf8Max    byte
	number     []byte
	numberStep int
	literal    []byte
	need       string
	capture    []byte
	capturing  bool
}

func newJSONUsageObserver(path string) *jsonUsageObserver {
	return &jsonUsageObserver{path: path}
}

// Feed examines only bytes already consumed by the proxy; it never reads ahead.
func (o *jsonUsageObserver) Feed(p []byte) {
	if o.failed {
		return
	}
	for _, b := range p {
		if o.capturing {
			o.capture = append(o.capture, b)
			if len(o.capture) > 4096 {
				o.failed = true
				return
			}
		}
		if o.lex == 'n' && isJSONDelimiter(b) {
			if !o.finishNumber() {
				o.failed = true
				return
			}
			if !o.normal(b) {
				o.failed = true
				return
			}
			continue
		}
		if !o.consume(b) {
			o.failed = true
			return
		}
	}
}

// Finish validates the complete document and publishes only fully validated usage.
func (o *jsonUsageObserver) Finish() TokenUsage {
	if o.lex == 'n' && !o.finishNumber() {
		o.failed = true
	}
	if o.failed || !o.rootSeen || !o.rootDone || len(o.stack) != 0 || o.lex != 0 || !o.usageSeen || o.capturing || usageKeys(o.path) == nil {
		return unavailable()
	}
	return decodeUsageCapture(o.path, o.capture)
}

func (o *jsonUsageObserver) consume(b byte) bool {
	switch o.lex {
	case 's':
		return o.stringByte(b)
	case 'n':
		return o.numberByte(b)
	case 'l':
		if len(o.literal) >= len(o.need) || b != o.need[len(o.literal)] {
			return false
		}
		o.literal = append(o.literal, b)
		if len(o.literal) == len(o.need) {
			o.lex = 0
			o.completeValue()
		}
		return true
	default:
		return o.normal(b)
	}
}

func (o *jsonUsageObserver) normal(b byte) bool {
	if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
		return true
	}
	if o.rootDone {
		return false
	}
	if len(o.stack) == 0 {
		if o.rootSeen || b != '{' {
			return false
		}
		o.rootSeen = true
		o.stack = append(o.stack, jsonUsageFrame{kind: jsonObject, state: objKeyOrEnd, root: true})
		return true
	}

	i := len(o.stack) - 1
	f := &o.stack[i]
	switch f.kind {
	case jsonObject:
		switch f.state {
		case objKeyOrEnd:
			if b == '}' {
				return o.closeContainer()
			}
			if b != '"' {
				return false
			}
			o.startString(true)
			return true
		case objKey:
			if b != '"' {
				return false
			}
			o.startString(true)
			return true
		case objColon:
			if b != ':' {
				return false
			}
			f.state = objValue
			return true
		case objValue:
			return o.startValue(b)
		case objCommaEnd:
			if b == ',' {
				f.state = objKey
				return true
			}
			if b == '}' {
				return o.closeContainer()
			}
			return false
		}
	case jsonArray:
		switch f.state {
		case arrValueOrEnd:
			if b == ']' {
				return o.closeContainer()
			}
			return o.startValue(b)
		case arrValue:
			return o.startValue(b)
		case arrCommaEnd:
			if b == ',' {
				f.state = arrValue
				return true
			}
			if b == ']' {
				return o.closeContainer()
			}
			return false
		}
	}
	return false
}

func (o *jsonUsageObserver) startValue(b byte) bool {
	parent := &o.stack[len(o.stack)-1]
	selected := parent.root && parent.wantsUsage
	if selected {
		parent.wantsUsage = false
		if b != '{' {
			return false
		}
		o.capturing = true
		o.capture = append(o.capture[:0], '{')
	} else if o.capturing {
		// The current byte was captured at Feed entry.
	}
	if parent.kind == jsonObject {
		parent.state = objCommaEnd
	} else {
		parent.state = arrCommaEnd
	}
	switch b {
	case '{':
		if len(o.stack) >= 64 {
			return false
		}
		frame := jsonUsageFrame{kind: jsonObject, state: objKeyOrEnd, usage: selected}
		o.stack = append(o.stack, frame)
	case '[':
		if len(o.stack) >= 64 || selected {
			return false
		}
		o.stack = append(o.stack, jsonUsageFrame{kind: jsonArray, state: arrValueOrEnd})
	case '"':
		o.startString(false)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		o.lex = 'n'
		o.number = o.number[:0]
		o.numberStep = -1
		if !o.numberByte(b) {
			return false
		}
	case 't':
		o.startLiteral("true", b)
	case 'f':
		o.startLiteral("false", b)
	case 'n':
		o.startLiteral("null", b)
	default:
		return false
	}
	return true
}

func (o *jsonUsageObserver) startString(key bool) {
	o.lex = 's'
	o.stringKey = key
	o.stringRaw = o.stringRaw[:0]
	o.stringLong = false
	o.escape = false
	o.unicode = 0
	o.utf8Need = 0
	o.utf8Min = 0x80
	o.utf8Max = 0xbf
	if key {
		o.stringRaw = append(o.stringRaw, '"')
	}
}

func (o *jsonUsageObserver) stringByte(b byte) bool {
	if !o.validUTF8Byte(b) {
		return false
	}
	if o.stringKey && !o.stringLong {
		if len(o.stringRaw) >= 64 {
			o.stringLong = true
			o.stringRaw = o.stringRaw[:0]
		} else {
			o.stringRaw = append(o.stringRaw, b)
		}
	}
	if o.unicode > 0 {
		if !isHex(b) {
			return false
		}
		o.unicode--
		return true
	}
	if o.escape {
		o.escape = false
		switch b {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			return true
		case 'u':
			o.unicode = 4
			return true
		default:
			return false
		}
	}
	switch b {
	case '\\':
		o.escape = true
	case '"':
		o.lex = 0
		if o.stringKey {
			f := &o.stack[len(o.stack)-1]
			key := ""
			if !o.stringLong {
				var decoded string
				if json.Unmarshal(o.stringRaw, &decoded) == nil && len(decoded) <= 64 {
					key = decoded
				}
			}
			f.state = objColon
			if f.root && key == "usage" {
				if o.usageSeen {
					return false
				}
				o.usageSeen = true
				f.wantsUsage = true
			}
		} else {
			o.completeValue()
		}
	default:
		if b < 0x20 {
			return false
		}
	}
	return true
}

func (o *jsonUsageObserver) validUTF8Byte(b byte) bool {
	if o.utf8Need > 0 {
		if b < o.utf8Min || b > o.utf8Max {
			return false
		}
		o.utf8Need--
		o.utf8Min, o.utf8Max = 0x80, 0xbf
		return true
	}
	switch {
	case b < 0x80:
		return true
	case b >= 0xc2 && b <= 0xdf:
		o.utf8Need = 1
	case b == 0xe0:
		o.utf8Need, o.utf8Min = 2, 0xa0
	case b >= 0xe1 && b <= 0xec || b >= 0xee && b <= 0xef:
		o.utf8Need = 2
	case b == 0xed:
		o.utf8Need, o.utf8Max = 2, 0x9f
	case b == 0xf0:
		o.utf8Need, o.utf8Min = 3, 0x90
	case b >= 0xf1 && b <= 0xf3:
		o.utf8Need = 3
	case b == 0xf4:
		o.utf8Need, o.utf8Max = 3, 0x8f
	default:
		return false
	}
	return true
}

func (o *jsonUsageObserver) startLiteral(text string, first byte) {
	o.lex = 'l'
	o.need = text
	o.literal = append(o.literal[:0], first)
	if len(text) == 1 {
		o.lex = 0
		o.completeValue()
	}
}

func (o *jsonUsageObserver) numberByte(b byte) bool {
	if len(o.number) >= 20 {
		return false
	}
	o.number = append(o.number, b)
	switch o.numberStep {
	case -1:
		if b == '-' {
			o.numberStep = 0
		} else if b == '0' {
			o.numberStep = 1
		} else if b >= '1' && b <= '9' {
			o.numberStep = 2
		} else {
			return false
		}
	case 0:
		if b == '0' {
			o.numberStep = 1
		} else if b >= '1' && b <= '9' {
			o.numberStep = 2
		} else {
			return false
		}
	case 1:
		if b == '.' {
			o.numberStep = 3
		} else if b == 'e' || b == 'E' {
			o.numberStep = 5
		} else if b >= '0' && b <= '9' {
			return false
		} else {
			return false
		}
	case 2:
		if b >= '0' && b <= '9' {
			return true
		} else if b == '.' {
			o.numberStep = 3
		} else if b == 'e' || b == 'E' {
			o.numberStep = 5
		} else {
			return false
		}
	case 3:
		if b < '0' || b > '9' {
			return false
		}
		o.numberStep = 4
	case 4:
		if b >= '0' && b <= '9' {
			return true
		} else if b == 'e' || b == 'E' {
			o.numberStep = 5
		} else {
			return false
		}
	case 5:
		if b == '+' || b == '-' {
			o.numberStep = 6
		} else if b >= '0' && b <= '9' {
			o.numberStep = 7
		} else {
			return false
		}
	case 6:
		if b < '0' || b > '9' {
			return false
		}
		o.numberStep = 7
	case 7:
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

func (o *jsonUsageObserver) finishNumber() bool {
	switch o.numberStep {
	case 1, 2, 4, 7:
		o.lex = 0
		o.completeValue()
		return true
	default:
		return false
	}
}

func (o *jsonUsageObserver) completeValue() {
	if len(o.stack) == 0 {
		o.rootDone = true
		return
	}
	// Container parents are marked complete when the container begins. Scalars
	// need the same transition; their parent is the current top frame.
	f := &o.stack[len(o.stack)-1]
	if f.kind == jsonObject && f.state == objValue {
		f.state = objCommaEnd
	} else if f.kind == jsonArray && (f.state == arrValue || f.state == arrValueOrEnd) {
		f.state = arrCommaEnd
	}
}

func (o *jsonUsageObserver) closeContainer() bool {
	f := o.stack[len(o.stack)-1]
	if f.usage {
		o.capturing = false
	}
	o.stack = o.stack[:len(o.stack)-1]
	if len(o.stack) == 0 {
		o.rootDone = true
	}
	return true
}

func isJSONDelimiter(b byte) bool {
	return b == ',' || b == ']' || b == '}' || b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func decodeUsageCapture(path string, raw []byte) TokenUsage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return unavailable()
	}
	keys := usageKeys(path)
	seen := make(map[string]bool, len(keys))
	values := make(map[string]*int64, len(keys))
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return unavailable()
		}
		key, ok := t.(string)
		if !ok {
			return unavailable()
		}
		if _, recognized := keys[key]; !recognized {
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return unavailable()
			}
			continue
		}
		if seen[key] {
			return unavailable()
		}
		seen[key] = true
		value, err := dec.Token()
		if err != nil {
			return unavailable()
		}
		if value == nil {
			continue
		}
		number, ok := value.(json.Number)
		if !ok {
			return unavailable()
		}
		text := number.String()
		if len(text) > 20 || strings.HasPrefix(text, "-") || strings.ContainsAny(text, ".eE+") {
			return unavailable()
		}
		count, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return unavailable()
		}
		values[key] = &count
	}
	if _, err := dec.Token(); err != nil {
		return unavailable()
	}
	if _, err := dec.Token(); err != io.EOF {
		return unavailable()
	}
	var result TokenUsage
	for key, direction := range keys {
		count := values[key]
		switch direction {
		case "in":
			result.Input = count
		case "out":
			result.Output = count
		case "total":
			result.Total = count
		}
	}
	if result.Total == nil && result.Input != nil && result.Output != nil && *result.Input <= math.MaxInt64-*result.Output {
		total := *result.Input + *result.Output
		result.Total = &total
	}
	if result.Input == nil && result.Output == nil && result.Total == nil {
		return unavailable()
	}
	if path == "/v1/embeddings" {
		if result.Input != nil && result.Total != nil {
			result.Status = "reported"
		} else {
			result.Status = "partial"
		}
	} else if result.Input != nil && result.Output != nil {
		result.Status = "reported"
	} else {
		result.Status = "partial"
	}
	return result
}
