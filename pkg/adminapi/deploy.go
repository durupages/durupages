// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package adminapi

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/durupages/durupages/pkg/bundle"
	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/provider"
)

// Names that must never be mistaken for the "single leading directory" that
// clients produce with `tar czf - mydir`: they are build output content.
const (
	nameWorker   = "_worker.js"
	dirFunctions = "functions"
)

// apiError is an error carrying the HTTP status and envelope code to report.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// newAPIError builds an apiError with a formatted message.
func newAPIError(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

// writeAPIError renders err, defaulting to 500 for a plain error.
func writeAPIError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeError(w, ae.status, ae.code, "%s", ae.msg)
		return
	}
	writeError(w, http.StatusInternalServerError, codeInternal, "%v", err)
}

// deployResponse is the 201 body of a deployment upload.
type deployResponse struct {
	DeploymentID string          `json:"deploymentId"`
	PageID       string          `json:"pageId"`
	TenantID     string          `json:"tenantId"`
	Activated    bool            `json:"activated"`
	Manifest     manifestSummary `json:"manifest"`
}

// manifestSummary is the deployment manifest reduced to what a deploying
// client wants to see; the full manifest lives in Storage.
type manifestSummary struct {
	Version          int    `json:"version"`
	HasWorker        bool   `json:"hasWorker"`
	StaticFileCount  int    `json:"staticFileCount"`
	StaticTotalBytes int64  `json:"staticTotalBytes"`
	RedirectCount    int    `json:"redirectCount"`
	HeaderRuleCount  int    `json:"headerRuleCount"`
	CompatDate       string `json:"compatDate,omitempty"`
}

// handleCreateDeployment serves POST /v1/pages/{pageId}/deployments.
//
// The request body is a tar stream of a wrangler build output directory,
// optionally gzipped. Compression is auto-detected from the gzip magic bytes,
// so Content-Type (application/x-tar, application/gzip, application/x-gzip) is
// advisory only. Query parameters:
//
//	deploymentId  optional; defaults to dep-<unix-nano>
//	activate      optional bool; defaults to true
//
// The page must already exist. Its tenant is taken from the stored page, never
// from the client.
func (s *Server) handleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	if s.storage == nil {
		writeNotImplemented(w, "object storage")
		return
	}
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}

	q := r.URL.Query()
	deploymentID := q.Get("deploymentId")
	if deploymentID == "" {
		deploymentID = fmt.Sprintf("dep-%d", s.now().UnixNano())
	}
	if !validID(deploymentID) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"invalid deploymentId %q", deploymentID)
		return
	}
	activate := true
	if v := q.Get("activate"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest,
				"invalid activate %q: want a boolean", v)
			return
		}
		activate = parsed
	}

	page, err := s.provider.GetPage(r.Context(), pageID)
	if err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}

	root, cleanup, err := s.extractUpload(w, r)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}

	scanned, err := bundle.Scan(root, bundle.ScanOptions{
		TenantID:     page.TenantID,
		PageID:       pageID,
		DeploymentID: deploymentID,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidBundle, "%v", err)
		return
	}
	if err := bundle.Upload(r.Context(), s.storage, scanned); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "%v", err)
		return
	}

	d := provider.Deployment{ID: deploymentID, PageID: pageID, CreatedAt: s.now().UTC()}
	if err := admin.CreateDeployment(r.Context(), d); err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	if activate {
		if err := admin.SetActiveDeployment(r.Context(), pageID, deploymentID); err != nil {
			writeProviderError(w, err, "page %q does not exist", pageID)
			return
		}
	}

	writeJSON(w, http.StatusCreated, deployResponse{
		DeploymentID: deploymentID,
		PageID:       pageID,
		TenantID:     page.TenantID,
		Activated:    activate,
		Manifest:     summarize(scanned.Manifest),
	})
}

// summarize reduces a manifest to the upload response summary.
func summarize(m *manifest.Manifest) manifestSummary {
	var total int64
	for _, e := range m.Static {
		total += e.Size
	}
	return manifestSummary{
		Version:          m.Version,
		HasWorker:        m.HasWorker,
		StaticFileCount:  len(m.Static),
		StaticTotalBytes: total,
		RedirectCount:    len(m.Redirects),
		HeaderRuleCount:  len(m.Headers),
		CompatDate:       m.Compat.Date,
	}
}

// extractUpload streams the request body into a fresh temporary directory and
// returns the build output root inside it. The caller must always invoke
// cleanup, which is non-nil as soon as the directory exists.
//
// The directory is created under Options.TempDir (os.TempDir() when unset);
// a deployment with a read-only root filesystem points that option at a
// writable volume.
func (s *Server) extractUpload(w http.ResponseWriter, r *http.Request) (root string, cleanup func(), err error) {
	tmp, err := os.MkdirTemp(s.tempDir, "durupages-upload-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }

	body := http.MaxBytesReader(w, r.Body, s.maxUpload)
	br := bufio.NewReader(body)

	var src io.Reader = br
	magic, perr := br.Peek(2)
	if perr != nil && !errors.Is(perr, io.EOF) && !errors.Is(perr, bufio.ErrBufferFull) {
		return "", cleanup, classifyReadError(perr)
	}
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, gerr := gzip.NewReader(br)
		if gerr != nil {
			return "", cleanup, newAPIError(http.StatusBadRequest, codeInvalidRequest,
				"invalid gzip stream: %v", gerr)
		}
		defer zr.Close()
		src = zr
	}

	if err := s.untar(src, tmp); err != nil {
		return "", cleanup, err
	}
	return buildRoot(tmp), cleanup, nil
}

// untar extracts a tar stream into dest, rejecting anything that could escape
// dest and enforcing the total extracted-size cap.
func (s *Server) untar(src io.Reader, dest string) error {
	tr := tar.NewReader(src)
	remaining := s.maxExtracted
	files := 0

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return classifyReadError(err)
		}

		rel, err := sanitizeTarPath(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue // the archive root entry ("./")
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", rel, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create %s: %w", path.Dir(rel), err)
			}
			n, err := writeFile(target, tr, remaining)
			if err != nil {
				return err
			}
			remaining -= n
			files++
		case tar.TypeSymlink, tar.TypeLink:
			return newAPIError(http.StatusBadRequest, codeInvalidRequest,
				"archive entry %q is a link; links are not supported", hdr.Name)
		default:
			// Devices, fifos, sockets and metadata-only entries carry nothing
			// deployable; skip them.
			continue
		}
	}

	if files == 0 {
		return newAPIError(http.StatusBadRequest, codeInvalidRequest,
			"archive contains no files")
	}
	return nil
}

// writeFile copies at most remaining bytes from r into path, reporting how
// many were written. Exceeding the budget is a 413.
func writeFile(target string, r io.Reader, remaining int64) (int64, error) {
	if remaining <= 0 {
		return 0, tooLargeError()
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		// A duplicated entry simply overwrites the previous one.
		f, err = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	}
	if err != nil {
		return 0, fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(r, remaining+1))
	if err != nil {
		return n, classifyReadError(err)
	}
	if n > remaining {
		return n, tooLargeError()
	}
	return n, nil
}

// tooLargeError reports that the extracted archive blew the size budget.
func tooLargeError() error {
	return newAPIError(http.StatusRequestEntityTooLarge, codeTooLarge,
		"extracted archive exceeds the configured size limit")
}

// classifyReadError maps a body/stream read failure onto an API error: an
// oversized request body becomes 413, a malformed archive 400.
func classifyReadError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return newAPIError(http.StatusRequestEntityTooLarge, codeTooLarge,
			"request body exceeds %d bytes", tooLarge.Limit)
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return err
	}
	return newAPIError(http.StatusBadRequest, codeInvalidRequest, "invalid archive: %v", err)
}

// sanitizeTarPath validates a tar entry name and returns it as a clean
// slash-separated relative path. It returns "" for the archive root entry
// ("." or "./"), and an error for anything that could escape the destination:
// absolute paths, Windows drive/UNC paths and ".." segments.
func sanitizeTarPath(name string) (string, error) {
	reject := func(reason string) error {
		return newAPIError(http.StatusBadRequest, codeInvalidRequest,
			"archive entry %q is rejected: %s", name, reason)
	}
	n := strings.TrimSuffix(name, "/")
	if n == "" {
		if name == "/" {
			return "", reject("absolute path")
		}
		return "", nil // "" or "./" -> archive root
	}
	if strings.HasPrefix(n, "/") || filepath.IsAbs(n) || filepath.VolumeName(n) != "" {
		return "", reject("absolute path")
	}
	if strings.ContainsAny(n, "\\\x00") {
		return "", reject("illegal character in path")
	}
	clean := path.Clean(n)
	if clean == "." {
		return "", nil
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return "", reject("path traversal")
		}
	}
	return clean, nil
}

// buildRoot tolerates a single leading directory: clients commonly run
// `tar czf - dist`, which nests everything under dist/. When the extracted
// tree holds exactly one entry and it is a directory that is not itself build
// output (_worker.js/ or functions/), that directory is the build root.
func buildRoot(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		return dir
	}
	e := entries[0]
	if !e.IsDir() || e.Name() == nameWorker || e.Name() == dirFunctions {
		return dir
	}
	return filepath.Join(dir, e.Name())
}
