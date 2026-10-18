package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string { return fmt.Sprintf("remote HTTP %d", e.Status) }

var apiClient = &http.Client{Timeout: 40 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}

func api(ctx context.Context, method, base, path, token string, body any, out any, headers map[string]string) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(jsonBytes(body))
	}
	req, e := http.NewRequestWithContext(ctx, method, base+path, rd)
	if e != nil {
		return stateError("HTTP request construction")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, e := apiClient.Do(req)
	if e != nil {
		return errors.New("remote endpoint unavailable or timed out")
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if e != nil || len(b) > 8<<20 {
		return errors.New("remote response unreadable or exceeds 8 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var code struct {
			Code string `json:"errcode"`
		}
		json.Unmarshal(b, &code)
		if len(code.Code) > 64 {
			code.Code = ""
		}
		return &APIError{Status: resp.StatusCode, Code: code.Code}
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return errors.New("remote JSON response invalid")
	}
	return nil
}
func httpStatus(e error, n int) bool { var a *APIError; return errors.As(e, &a) && a.Status == n }
