// SPDX-License-Identifier: GPL-3.0-only

// Package aptly is a small client for the aptly 1.6 REST API, covering only
// what aptrix needs.
package aptly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrReadOnly is returned when a read-only client is asked to modify state.
var ErrReadOnly = errors.New("aptly client is read-only")

// APIError is a non-2xx response from aptly.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("aptly %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound reports whether err is an aptly 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// Client talks to one aptly API server.
type Client struct {
	base     *url.URL
	http     *http.Client
	auth     func(*http.Request)
	readOnly bool
}

// Options configure a Client.
type Options struct {
	// ReadOnly makes every non-GET request fail before it is sent.
	ReadOnly bool
	// Token is sent as "Authorization: Bearer <token>".
	Token string
	// Username/Password are sent as HTTP basic auth.
	Username, Password string
}

// New creates a client for baseURL (e.g. http://127.0.0.1:8080).
func New(baseURL string, o Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid aptly url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid aptly url %q: need http:// or https://", baseURL)
	}
	c := &Client{base: u, readOnly: o.ReadOnly}
	// No overall timeout: publishing a large repo is synchronous and slow.
	// Callers cancel through the context instead.
	c.http = &http.Client{Transport: &guard{next: http.DefaultTransport, readOnly: o.ReadOnly}}
	switch {
	case o.Token != "":
		c.auth = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+o.Token) }
	case o.Username != "":
		c.auth = func(r *http.Request) { r.SetBasicAuth(o.Username, o.Password) }
	}
	return c, nil
}

// IsLoopback reports whether the client points at a loopback address.
func (c *Client) IsLoopback() bool {
	host := c.base.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// URL returns the base URL.
func (c *Client) URL() string { return c.base.String() }

// guard enforces read-only mode at the transport, so no code path can slip
// a write through.
type guard struct {
	next     http.RoundTripper
	readOnly bool
}

func (g *guard) RoundTrip(r *http.Request) (*http.Response, error) {
	if g.readOnly && r.Method != http.MethodGet && r.Method != http.MethodHead {
		return nil, fmt.Errorf("%w: refusing %s %s", ErrReadOnly, r.Method, r.URL.Path)
	}
	return g.next.RoundTrip(r)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	// path is already escaped by the callers.
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+"/api"+path, body)
	if err != nil {
		return err
	}
	u := req.URL
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.auth != nil {
		c.auth(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &APIError{Method: method, Path: u.Path, Status: resp.StatusCode, Message: msg}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("aptly %s %s: decoding response: %w", method, u.Path, err)
		}
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, "", out)
}

func (c *Client) send(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	return c.do(ctx, method, path, body, "application/json", out)
}

// PublishPath escapes "storage:prefix" for use in /api/publish/... URLs
// (aptly maps "_" to "/" and "__" to "_").
func PublishPath(storage, prefix string) string {
	if prefix == "" {
		prefix = "."
	}
	p := strings.ReplaceAll(prefix, "_", "__")
	p = strings.ReplaceAll(p, "/", "_")
	if storage != "" {
		p = storage + ":" + p
	}
	return url.PathEscape(p)
}

func distPath(dist string) string {
	return url.PathEscape(strings.ReplaceAll(strings.ReplaceAll(dist, "_", "__"), "/", "_"))
}

// ---- types ----

// Repo is an aptly local repository.
type Repo struct {
	Name                string `json:"Name"`
	Comment             string `json:"Comment"`
	DefaultDistribution string `json:"DefaultDistribution"`
	DefaultComponent    string `json:"DefaultComponent"`
}

// Source maps a component to a local repo (or snapshot).
type Source struct {
	Component string `json:"Component"`
	Name      string `json:"Name"`
}

// Published is a published repository (one storage/prefix/distribution).
type Published struct {
	Storage       string   `json:"Storage"`
	Prefix        string   `json:"Prefix"`
	Distribution  string   `json:"Distribution"`
	SourceKind    string   `json:"SourceKind"`
	Sources       []Source `json:"Sources"`
	Architectures []string `json:"Architectures"`
	Origin        string   `json:"Origin"`
	Label         string   `json:"Label"`
}

// Package is the subset of package fields aptrix uses.
type Package struct {
	Key          string `json:"Key"`
	Package      string `json:"Package"`
	Version      string `json:"Version"`
	Architecture string `json:"Architecture"`
	Filename     string `json:"Filename"`
}

// Signing is sent with publish create/update.
type Signing struct {
	Skip           bool   `json:"Skip,omitempty"`
	GpgKey         string `json:"GpgKey,omitempty"`
	PassphraseFile string `json:"PassphraseFile,omitempty"`
	Batch          bool   `json:"Batch,omitempty"`
}

// ImportResult is returned when adding uploaded files to a repo.
type ImportResult struct {
	FailedFiles []string `json:"FailedFiles"`
	Report      struct {
		Warnings []string `json:"Warnings"`
		Added    []string `json:"Added"`
		Removed  []string `json:"Removed"`
	} `json:"Report"`
}

// ---- read ----

// Version returns the aptly server version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"Version"`
	}
	err := c.get(ctx, "/version", &v)
	return strings.TrimSpace(v.Version), err
}

// Repos lists local repositories.
func (c *Client) Repos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	return out, c.get(ctx, "/repos", &out)
}

// Packages lists packages of a local repo with details.
func (c *Client) Packages(ctx context.Context, repo string) ([]Package, error) {
	var out []Package
	return out, c.get(ctx, "/repos/"+url.PathEscape(repo)+"/packages?format=details", &out)
}

// PackageKeys lists package keys of a local repo (cheaper than details).
func (c *Client) PackageKeys(ctx context.Context, repo string) ([]string, error) {
	var out []string
	return out, c.get(ctx, "/repos/"+url.PathEscape(repo)+"/packages", &out)
}

// Publishes lists all published repositories.
func (c *Client) Publishes(ctx context.Context) ([]Published, error) {
	var out []Published
	return out, c.get(ctx, "/publish", &out)
}

// PendingSources returns staged source changes of a publish, or nil if none.
func (c *Client) PendingSources(ctx context.Context, storage, prefix, dist string) ([]Source, error) {
	var out []Source
	err := c.get(ctx, "/publish/"+PublishPath(storage, prefix)+"/"+distPath(dist)+"/sources", &out)
	if IsNotFound(err) {
		return nil, nil
	}
	return out, err
}

// ---- write ----

// CreateRepo creates a local repo.
func (c *Client) CreateRepo(ctx context.Context, r Repo) error {
	return c.send(ctx, http.MethodPost, "/repos", r, nil)
}

// DropRepo deletes a local repo. Note: aptly deletes repos that still contain
// packages; callers must check emptiness first. force=1 only skips aptly's
// "repo has snapshots" check (snapshots survive and keep their packages);
// aptly still refuses to drop a published repo.
func (c *Client) DropRepo(ctx context.Context, name string) error {
	return c.send(ctx, http.MethodDelete, "/repos/"+url.PathEscape(name)+"?force=1", nil, nil)
}

// DeletePackages removes packages (by key) from a local repo.
func (c *Client) DeletePackages(ctx context.Context, repo string, keys []string) error {
	return c.send(ctx, http.MethodDelete, "/repos/"+url.PathEscape(repo)+"/packages",
		map[string][]string{"PackageRefs": keys}, nil)
}

// Upload uploads files into a temporary upload directory.
func (c *Client) Upload(ctx context.Context, dir string, files []string) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for _, f := range files {
				fh, err := os.Open(f)
				if err != nil {
					return err
				}
				part, err := mw.CreateFormFile("file", filepath.Base(f))
				if err == nil {
					_, err = io.Copy(part, fh)
				}
				fh.Close()
				if err != nil {
					return err
				}
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	return c.do(ctx, http.MethodPost, "/files/"+url.PathEscape(dir), pr, mw.FormDataContentType(), nil)
}

// DeleteUploadDir removes an upload directory.
func (c *Client) DeleteUploadDir(ctx context.Context, dir string) error {
	err := c.send(ctx, http.MethodDelete, "/files/"+url.PathEscape(dir), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ImportDir adds all files of an upload dir to a repo. The dir is kept
// (noRemove=1) so it can be imported into several repos. A response that
// lists failed files is returned as an error.
func (c *Client) ImportDir(ctx context.Context, repo, dir string, forceReplace bool) (*ImportResult, error) {
	q := "?noRemove=1"
	if forceReplace {
		q += "&forceReplace=1"
	}
	var res ImportResult
	err := c.send(ctx, http.MethodPost, "/repos/"+url.PathEscape(repo)+"/file/"+url.PathEscape(dir)+q, nil, &res)
	if err != nil {
		return nil, err
	}
	if len(res.FailedFiles) > 0 {
		names := make([]string, len(res.FailedFiles))
		for i, f := range res.FailedFiles {
			names[i] = filepath.Base(f)
		}
		return &res, fmt.Errorf("aptly could not add %s to %s: %s", strings.Join(names, ", "), repo, strings.Join(res.Report.Warnings, "; "))
	}
	return &res, nil
}

// PublishCreate is the body of a new local-repo publish.
type PublishCreate struct {
	SourceKind    string   `json:"SourceKind"`
	Sources       []Source `json:"Sources"`
	Distribution  string   `json:"Distribution"`
	Architectures []string `json:"Architectures"`
	Origin        string   `json:"Origin,omitempty"`
	Label         string   `json:"Label,omitempty"`
	Signing       Signing  `json:"Signing"`
}

// CreatePublish publishes local repos as a new distribution.
func (c *Client) CreatePublish(ctx context.Context, storage, prefix string, p PublishCreate) error {
	p.SourceKind = "local"
	return c.send(ctx, http.MethodPost, "/publish/"+PublishPath(storage, prefix), p, nil)
}

// DropPublish removes a published distribution.
func (c *Client) DropPublish(ctx context.Context, storage, prefix, dist string) error {
	return c.send(ctx, http.MethodDelete, "/publish/"+PublishPath(storage, prefix)+"/"+distPath(dist), nil, nil)
}

// StageAddSource stages adding a component to a publish.
func (c *Client) StageAddSource(ctx context.Context, storage, prefix, dist string, s Source) error {
	return c.send(ctx, http.MethodPost, "/publish/"+PublishPath(storage, prefix)+"/"+distPath(dist)+"/sources", s, nil)
}

// StageReplaceSource stages pointing an existing component at another repo.
func (c *Client) StageReplaceSource(ctx context.Context, storage, prefix, dist string, s Source) error {
	return c.send(ctx, http.MethodPut, "/publish/"+PublishPath(storage, prefix)+"/"+distPath(dist)+"/sources/"+url.PathEscape(s.Component), s, nil)
}

// StageRemoveSource stages removing a component from a publish.
func (c *Client) StageRemoveSource(ctx context.Context, storage, prefix, dist, component string) error {
	return c.send(ctx, http.MethodDelete, "/publish/"+PublishPath(storage, prefix)+"/"+distPath(dist)+"/sources/"+url.PathEscape(component), nil, nil)
}

// DropPendingSources discards staged source changes.
func (c *Client) DropPendingSources(ctx context.Context, storage, prefix, dist string) error {
	return c.send(ctx, http.MethodDelete, "/publish/"+PublishPath(storage, prefix)+"/"+distPath(dist)+"/sources", nil, nil)
}

// UpdatePublish applies staged source changes (if any) and republishes the
// current contents of the source repos. forceOverwrite lets aptly replace a
// pool file whose content changed (needed after a forced package replace).
func (c *Client) UpdatePublish(ctx context.Context, storage, prefix, dist string, s Signing, forceOverwrite bool) error {
	return c.send(ctx, http.MethodPost, "/publish/"+PublishPath(storage, prefix)+"/"+distPath(dist)+"/update",
		map[string]any{"Signing": s, "ForceOverwrite": forceOverwrite}, nil)
}

// ---- snapshots ----

// Snapshot is an aptly snapshot.
type Snapshot struct {
	Name        string    `json:"Name"`
	Description string    `json:"Description"`
	SourceKind  string    `json:"SourceKind"`
	CreatedAt   time.Time `json:"CreatedAt"`
}

// Snapshots lists all snapshots.
func (c *Client) Snapshots(ctx context.Context) ([]Snapshot, error) {
	var out []Snapshot
	return out, c.get(ctx, "/snapshots", &out)
}

// SnapshotPackageKeys lists the package keys of a snapshot.
func (c *Client) SnapshotPackageKeys(ctx context.Context, name string) ([]string, error) {
	var out []string
	return out, c.get(ctx, "/snapshots/"+url.PathEscape(name)+"/packages", &out)
}

// SnapshotRepo creates a snapshot of a local repo's current contents.
func (c *Client) SnapshotRepo(ctx context.Context, repo, name, description string) error {
	return c.send(ctx, http.MethodPost, "/repos/"+url.PathEscape(repo)+"/snapshots",
		map[string]string{"Name": name, "Description": description}, nil)
}

// DropSnapshot deletes a snapshot.
func (c *Client) DropSnapshot(ctx context.Context, name string) error {
	return c.send(ctx, http.MethodDelete, "/snapshots/"+url.PathEscape(name), nil, nil)
}

// AddPackages adds packages (by key) that aptly already knows, e.g. from a
// snapshot, to a local repo.
func (c *Client) AddPackages(ctx context.Context, repo string, keys []string) error {
	return c.send(ctx, http.MethodPost, "/repos/"+url.PathEscape(repo)+"/packages",
		map[string][]string{"PackageRefs": keys}, nil)
}

// DBCleanup removes packages no repo or snapshot references and deletes
// unreferenced pool files (aptly db cleanup). It affects the whole server.
func (c *Client) DBCleanup(ctx context.Context) error {
	return c.send(ctx, http.MethodPost, "/db/cleanup", nil, nil)
}
