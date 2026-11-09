package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// llama-swap v269 (9fbc47ce96d10160e95994c7a26513c972201a74) contracts:
// /health is unauthenticated plain-text OK; /v1/models is the OpenAI data[].id schema;
// /running contains running[].model objects. Keep these shapes pinned to v269.
type runtimeStatus struct {
	Enabled    bool      `json:"enabled"`
	Status     string    `json:"status"`
	Health     bool      `json:"healthy"`
	Configured []string  `json:"configured_models"`
	Running    []string  `json:"running_models"`
	SampledAt  time.Time `json:"sampled_at"`
}

func runtimeSnapshot(ctx context.Context, base, key string) runtimeStatus {
	out := runtimeStatus{Enabled: true, Status: "unavailable", SampledAt: time.Now().UTC(), Configured: []string{}, Running: []string{}}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(path string) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
		if err != nil {
			return nil, 0, err
		}
		if path != "/health" && key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil {
			return nil, resp.StatusCode, err
		}
		if len(body) > 1<<20 {
			return nil, resp.StatusCode, errRuntimeMalformed
		}
		return body, resp.StatusCode, nil
	}
	classify := func(status int, err error) {
		if errors.Is(err, errRuntimeMalformed) {
			out.Status = "malformed"
		} else if status == http.StatusUnauthorized {
			out.Status = "unauthorized"
		} else if err != nil || status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
			out.Status = "unavailable"
		} else {
			out.Status = "unsupported"
		}
	}
	type result struct {
		path   string
		body   []byte
		status int
		err    error
	}
	results := make(chan result, 3)
	for _, path := range []string{"/health", "/v1/models", "/running"} {
		go func(path string) { b, s, e := request(path); results <- result{path, b, s, e} }(path)
	}
	responses := make(map[string]result, 3)
	for range 3 {
		select {
		case v := <-results:
			responses[v.path] = v
		case <-ctx.Done():
			out.Status = "unavailable"
			return out
		}
	}
	for _, path := range []string{"/health", "/v1/models", "/running"} {
		v := responses[path]
		if v.err != nil || v.status != http.StatusOK {
			classify(v.status, v.err)
			return out
		}
	}
	health := responses["/health"].body
	if strings.TrimSpace(string(health)) != "OK" {
		out.Status = "malformed"
		return out
	}
	out.Health = true
	modelsBody := responses["/v1/models"].body
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(modelsBody, &models) != nil || models.Data == nil || len(models.Data) > 256 {
		out.Status = "malformed"
		return out
	}
	runningBody := responses["/running"].body
	var running struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if json.Unmarshal(runningBody, &running) != nil || running.Running == nil || len(running.Running) > 256 {
		out.Status = "malformed"
		return out
	}
	for _, m := range models.Data {
		if m.ID == "" || len(m.ID) > 128 {
			out.Status = "malformed"
			return out
		}
		out.Configured = append(out.Configured, m.ID)
	}
	for _, m := range running.Running {
		if m.Model == "" || len(m.Model) > 128 || len(m.State) > 32 {
			out.Status = "malformed"
			return out
		}
		out.Running = append(out.Running, m.Model)
	}
	out.Status = "reachable"
	return out
}

var errRuntimeMalformed = errors.New("malformed runtime response")
