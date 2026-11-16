package runner

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, http: httpClient}
}
func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		var detail struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&detail)
		switch res.StatusCode {
		case 404:
			return nil, ErrNotFound
		case 409:
			if detail.Error == ErrNotReady.Error() {
				return nil, ErrNotReady
			}
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("runner HTTP %d: %s", res.StatusCode, detail.Error)
	}
	return res, nil
}
func (c *Client) Start(ctx context.Context, spec RunSpec) (Run, error) {
	res, err := c.request(ctx, "POST", "/v1/runs", spec)
	if err != nil {
		return Run{}, err
	}
	defer res.Body.Close()
	var run Run
	err = json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&run)
	return run, err
}
func (c *Client) Inspect(ctx context.Context, id string) (Run, error) {
	res, err := c.request(ctx, "GET", "/v1/runs/"+url.PathEscape(id), nil)
	if err != nil {
		return Run{}, err
	}
	defer res.Body.Close()
	var run Run
	err = json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&run)
	return run, err
}
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	res, err := c.request(ctx, "GET", "/v1/capabilities", nil)
	if err != nil {
		return Capabilities{}, err
	}
	defer res.Body.Close()
	var capabilities Capabilities
	err = json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&capabilities)
	return capabilities, err
}
func (c *Client) Stop(ctx context.Context, id string) (Run, error) {
	res, err := c.request(ctx, "POST", "/v1/runs/"+url.PathEscape(id)+"/stop", nil)
	if err != nil {
		return Run{}, err
	}
	defer res.Body.Close()
	var run Run
	err = json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&run)
	return run, err
}
func (c *Client) Collect(ctx context.Context, id string) (io.ReadCloser, error) {
	res, err := c.request(ctx, "GET", "/v1/runs/"+url.PathEscape(id)+"/artifacts", nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}
func (c *Client) Acknowledge(ctx context.Context, id string) error {
	res, err := c.request(ctx, "POST", "/v1/runs/"+url.PathEscape(id)+"/acknowledge", nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return nil
}

func (c *Client) ContextManifest(ctx context.Context, runID string) (ContextManifest, error) {
	res, err := c.request(ctx, "GET", "/internal/runner/context/"+url.PathEscape(runID)+"/manifest", nil)
	if err != nil {
		return ContextManifest{}, err
	}
	defer res.Body.Close()
	var manifest ContextManifest
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&manifest)
	return manifest, err
}

func (c *Client) ContextRef(ctx context.Context, runID, refID string) (string, error) {
	res, err := c.request(ctx, "GET", "/internal/runner/context/"+url.PathEscape(runID)+"/refs/"+url.PathEscape(refID), nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var body struct {
		Body string `json:"body"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body)
	return body.Body, err
}

type HTTPAuthorizer struct {
	client    *Client
	machineID string
}

func NewHTTPAuthorizer(baseURL, token, machineID string, httpClient *http.Client) *HTTPAuthorizer {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &HTTPAuthorizer{client: NewClient(baseURL, token, httpClient), machineID: machineID}
}
func (a *HTTPAuthorizer) callbackPath(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "machine_id=" + url.QueryEscape(a.machineID)
}
func (a *HTTPAuthorizer) Authorize(ctx context.Context, request PermitRequest) (Permit, error) {
	res, err := a.client.request(ctx, "POST", a.callbackPath("/internal/runner/authorize"), request)
	if err != nil {
		return Permit{}, err
	}
	defer res.Body.Close()
	var permit Permit
	err = json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&permit)
	if err != nil {
		return Permit{}, err
	}
	// A healthy callback is required at least once a minute even if the server issued a long grant.
	max := time.Now().Add(time.Minute)
	if permit.ExpiresAt.After(max) {
		permit.ExpiresAt = max
	}
	return permit, nil
}

func (a *HTTPAuthorizer) ContextManifest(ctx context.Context, runID string) (ContextManifest, error) {
	path := "/internal/runner/context/" + url.PathEscape(runID) + "/manifest"
	res, err := a.client.request(ctx, "GET", a.callbackPath(path), nil)
	if err != nil {
		return ContextManifest{}, err
	}
	defer res.Body.Close()
	var manifest ContextManifest
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&manifest)
	return manifest, err
}

func (a *HTTPAuthorizer) ContextRef(ctx context.Context, runID, refID string) (string, error) {
	path := "/internal/runner/context/" + url.PathEscape(runID) + "/refs/" + url.PathEscape(refID)
	res, err := a.client.request(ctx, "GET", a.callbackPath(path), nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var body struct {
		Body string `json:"body"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body)
	return body.Body, err
}

func (a *HTTPAuthorizer) ContextRefPage(ctx context.Context, runID, refID, cursor string) (ContextPage, error) {
	path := "/internal/runner/context/" + url.PathEscape(runID) + "/refs/" + url.PathEscape(refID) + "?protocol=3&cursor=" + url.QueryEscape(cursor)
	res, err := a.client.request(ctx, "GET", a.callbackPath(path), nil)
	if err != nil {
		return ContextPage{}, err
	}
	defer res.Body.Close()
	var page ContextPage
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&page)
	return page, err
}

func NewHandler(controller *Controller, token string) (http.Handler, error) {
	if len(token) < 32 {
		return nil, errors.New("runner service token must contain at least 32 characters")
	}
	mux := http.NewServeMux()
	ready := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		capabilities := controller.Capabilities(ctx)
		body := map[string]string{"status": "ready", "resource_limits": capabilities.ResourceLimits}
		if capabilities.Warning != "" {
			body["warning"] = capabilities.Warning
		}
		status := http.StatusOK
		if !capabilities.Ready {
			status = http.StatusServiceUnavailable
			body["status"], body["error"] = "not_ready", capabilities.Error
		}
		writeJSON(w, status, body)
	}
	mux.HandleFunc("GET /healthz", ready)
	mux.HandleFunc("GET /health/ready", ready)
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				writeError(w, 401, errors.New("unauthorized"))
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /v1/capabilities", auth(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, controller.Capabilities(ctx))
	}))
	mux.HandleFunc("POST /v1/runs", auth(func(w http.ResponseWriter, r *http.Request) {
		if controller.cfg.MaxRequestBytes > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, controller.cfg.MaxRequestBytes)
		}
		defer r.Body.Close()
		var spec RunSpec
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&spec); err != nil {
			writeError(w, 400, errors.New("invalid run specification"))
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			writeError(w, 400, errors.New("expected one JSON document"))
			return
		}
		run, err := controller.Start(r.Context(), spec)
		if err != nil {
			writeRunError(w, err)
			return
		}
		writeJSON(w, 202, run)
	}))
	mux.HandleFunc("GET /v1/runs/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		run, err := controller.Inspect(r.PathValue("id"))
		if err != nil {
			writeRunError(w, err)
			return
		}
		writeJSON(w, 200, run)
	}))
	mux.HandleFunc("POST /v1/runs/{id}/stop", auth(func(w http.ResponseWriter, r *http.Request) {
		run, err := controller.Stop(r.Context(), r.PathValue("id"))
		if err != nil {
			writeRunError(w, err)
			return
		}
		writeJSON(w, 200, run)
	}))
	mux.HandleFunc("GET /v1/runs/{id}/artifacts", auth(func(w http.ResponseWriter, r *http.Request) {
		file, err := controller.Collect(r.PathValue("id"))
		if err != nil {
			writeRunError(w, err)
			return
		}
		defer file.Close()
		st, err := file.Stat()
		if err != nil {
			writeRunError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="artifacts.tar.gz"`)
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "artifacts.tar.gz", st.ModTime(), file)
	}))
	mux.HandleFunc("POST /v1/runs/{id}/acknowledge", auth(func(w http.ResponseWriter, r *http.Request) {
		if err := controller.Acknowledge(r.PathValue("id")); err != nil {
			writeRunError(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"acknowledged": true})
	}))
	return mux, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": redact(err.Error())})
}
func writeRunError(w http.ResponseWriter, err error) {
	status := 422
	if errors.Is(err, ErrNotFound) {
		status = 404
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotReady) {
		status = 409
	}
	writeError(w, status, err)
}
