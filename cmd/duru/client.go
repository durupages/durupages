// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultTimeout bounds every admin API call. Deployment uploads stream a whole
// build output, so the default is generous.
const defaultTimeout = 10 * time.Minute

// maxErrorBodyBytes caps how much of an error response body is read back.
const maxErrorBodyBytes = 8 << 10

// maxResponseBytes caps how much of a successful response body is read back.
// Admin API responses are small JSON documents.
const maxResponseBytes = 8 << 20

// adminClient is a minimal client for the controller admin API. It is shared by
// `duru deploy --admin-url` and by every `duru <resource> ...` command.
type adminClient struct {
	base string
	http *http.Client
	// token, when non-empty, is sent as "Authorization: Bearer <token>".
	token string
	// headers are extra request headers; they override token-derived ones.
	headers http.Header
}

// newAdminClient builds a client for baseURL. A zero timeout means
// defaultTimeout.
func newAdminClient(baseURL, token string, headers http.Header, timeout time.Duration) *adminClient {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &adminClient{
		base:    strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
		token:   token,
		headers: headers,
	}
}

// apiStatusError is a non-2xx admin API response, with the error envelope
// decoded when the server sent one.
type apiStatusError struct {
	method     string
	path       string
	status     string // e.g. "404 Not Found"
	statusCode int
	code       string // envelope error code, e.g. "not_found"
	message    string // envelope error message
	body       string // raw body, when no envelope could be decoded
}

// Error renders the message the CLI shows: "<message> (<status>)".
func (e *apiStatusError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("%s (%s)", e.message, e.status)
	}
	return fmt.Sprintf("%s: %s", e.status, e.body)
}

// verbose prefixes the message with the request, the form `duru deploy` has
// always printed.
func (e *apiStatusError) verbose() error {
	return fmt.Errorf("%s %s: %s", e.method, e.path, e.Error())
}

// isNotFound reports whether err is a 404 from the admin API.
func isNotFound(err error) bool {
	var se *apiStatusError
	if errors.As(err, &se) {
		return se.statusCode == http.StatusNotFound
	}
	return false
}

// newStatusError decodes the API error envelope out of a non-2xx response.
func newStatusError(method, path string, resp *http.Response, body []byte) *apiStatusError {
	e := &apiStatusError{
		method: method, path: path,
		status: resp.Status, statusCode: resp.StatusCode,
		body: strings.TrimSpace(string(body)),
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		e.code = env.Error.Code
		e.message = env.Error.Message
	}
	return e
}

// apiError extracts a useful message from a non-2xx response. It keeps the
// message format used by `duru deploy`.
func apiError(method, path string, resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return newStatusError(method, path, resp, body).verbose()
}

// do issues one request, applying the bearer token and the extra headers.
// An explicit --header wins over --token for the same header name.
func (c *adminClient) do(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for name, values := range c.headers {
		for i, v := range values {
			if i == 0 {
				req.Header.Set(name, v)
			} else {
				req.Header.Add(name, v)
			}
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// request performs one call and returns the response body. A non-2xx status
// becomes an *apiStatusError.
func (c *adminClient) request(method, path string, body io.Reader, contentType string) ([]byte, error) {
	resp, err := c.do(method, path, body, contentType)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode/100 != 2 {
		return nil, newStatusError(method, path, resp, data)
	}
	if readErr != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", method, path, readErr)
	}
	return data, nil
}

// get performs a GET and returns the response body.
func (c *adminClient) get(path string) ([]byte, error) {
	return c.request(http.MethodGet, path, nil, "")
}

// delete performs a DELETE and discards the (empty) response body.
func (c *adminClient) delete(path string) error {
	_, err := c.request(http.MethodDelete, path, nil, "")
	return err
}

// postSpec marshals v as JSON, POSTs it and returns the response body.
func (c *adminClient) postSpec(path string, v any) ([]byte, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return c.request(http.MethodPost, path, bytes.NewReader(buf), "application/json")
}

// putSpec marshals v as JSON, PUTs it and returns the response body.
func (c *adminClient) putSpec(path string, v any) ([]byte, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return c.request(http.MethodPut, path, bytes.NewReader(buf), "application/json")
}

// exists reports whether a GET on path returns 200.
func (c *adminClient) exists(path string) (bool, error) {
	resp, err := c.do(http.MethodGet, path, nil, "")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
}

// postJSON POSTs payload and discards the response body, reporting a non-2xx
// status in the `duru deploy` message format.
func (c *adminClient) postJSON(path string, payload any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := c.do(http.MethodPost, path, bytes.NewReader(buf), "application/json")
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return apiError(http.MethodPost, path, resp)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// setCustomDomains replaces the page's whole custom domain set.
func (c *adminClient) setCustomDomains(pageID string, domains []string) ([]byte, error) {
	for i := range domains {
		domains[i] = strings.TrimSpace(domains[i])
	}
	if domains == nil {
		domains = []string{}
	}
	path := "/v1/pages/" + url.PathEscape(pageID) + "/custom-domains"
	return c.putSpec(path, map[string]any{"domains": domains})
}

// setSecrets replaces the page's whole secret map. A nil map is sent as an
// empty object, which removes every secret.
func (c *adminClient) setSecrets(pageID string, secrets map[string]string) ([]byte, error) {
	if secrets == nil {
		secrets = map[string]string{}
	}
	return c.putSpec(secretsPath(pageID), map[string]any{"secrets": secrets})
}

// patchSecrets upserts the named secrets and leaves the rest of the page's map
// alone (PATCH, as opposed to setSecrets' whole-map PUT). A nil value deletes
// that one secret.
func (c *adminClient) patchSecrets(pageID string, secrets map[string]*string) ([]byte, error) {
	if secrets == nil {
		secrets = map[string]*string{}
	}
	buf, err := json.Marshal(map[string]any{"secrets": secrets})
	if err != nil {
		return nil, err
	}
	return c.request(http.MethodPatch, secretsPath(pageID), bytes.NewReader(buf), "application/json")
}

// uploadDeployment streams dir as a tar.gz to the admin API and returns the
// raw JSON response body.
func (c *adminClient) uploadDeployment(dir, pageID, deploymentID string, activate bool) ([]byte, error) {
	if st, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("build output %q: %w", dir, err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("build output %q is not a directory", dir)
	}

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTarGz(pw, dir)) }()

	q := url.Values{}
	q.Set("activate", strconv.FormatBool(activate))
	if deploymentID != "" {
		q.Set("deploymentId", deploymentID)
	}
	path := "/v1/pages/" + url.PathEscape(pageID) + "/deployments?" + q.Encode()

	return c.request(http.MethodPost, path, pr, "application/gzip")
}

// writeTarGz writes dir's contents (relative paths, regular files only) as a
// gzipped tar. Symlinks and other irregular entries are skipped: the server
// rejects them anyway, and a Pages build output is plain files.
func writeTarGz(w io.Writer, dir string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)

		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{
				Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0),
			})
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(&tar.Header{
				Name: name, Typeflag: tar.TypeReg, Mode: 0o644,
				Size: info.Size(), ModTime: time.Unix(0, 0),
			}); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		default:
			return nil // skip symlinks, sockets, devices
		}
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
