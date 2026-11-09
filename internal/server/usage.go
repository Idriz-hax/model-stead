package server

import "io"

type TokenUsage struct {
	Input, Output, Total *int64
	Status               string
}

func ParseUsageJSON(path string, r io.Reader, encoding string) TokenUsage {
	if encoding != "" && encoding != "identity" || usageKeys(path) == nil {
		return unavailable()
	}
	o := newJSONUsageObserver(path)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			o.Feed(buf[:n])
		}
		if err != nil {
			if err != io.EOF {
				return unavailable()
			}
			break
		}
		if n == 0 {
			return unavailable()
		}
	}
	return o.Finish()
}

func usageKeys(path string) map[string]string {
	switch path {
	case "/v1/chat/completions", "/v1/completions":
		return map[string]string{"prompt_tokens": "in", "completion_tokens": "out", "total_tokens": "total"}
	case "/v1/responses":
		return map[string]string{"input_tokens": "in", "output_tokens": "out", "total_tokens": "total"}
	case "/v1/messages":
		return map[string]string{"input_tokens": "in", "output_tokens": "out"}
	case "/v1/embeddings":
		return map[string]string{"prompt_tokens": "in", "total_tokens": "total"}
	default:
		return nil
	}
}

func unavailable() TokenUsage { return TokenUsage{Status: "unavailable"} }

func isHex(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

type usageReadCloser struct {
	io.ReadCloser
	feed func([]byte)
}

func NewUsageReadCloser(r io.ReadCloser, feed func([]byte)) io.ReadCloser {
	return &usageReadCloser{ReadCloser: r, feed: feed}
}

func (r *usageReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 && r.feed != nil {
		r.feed(p[:n])
	}
	return n, err
}
