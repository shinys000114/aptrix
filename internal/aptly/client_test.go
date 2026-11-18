// SPDX-License-Identifier: GPL-3.0-only

package aptly

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublishPath(t *testing.T) {
	cases := []struct{ storage, prefix, want string }{
		{"", "", "."},
		{"", ".", "."},
		{"filesystem:public", "my-repo", "filesystem:public:my-repo"},
		{"filesystem:public", "my_repo", "filesystem:public:my__repo"},
		{"filesystem:public", "a/b", "filesystem:public:a_b"},
		{"s3:bucket", "a/b_c", "s3:bucket:a_b__c"},
	}
	for _, c := range cases {
		if got := PublishPath(c.storage, c.prefix); got != c.want {
			t.Errorf("PublishPath(%q, %q) = %q, want %q", c.storage, c.prefix, got, c.want)
		}
	}
}

func TestReadOnlyGuard(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Repos(ctx); err != nil {
		t.Fatalf("GET should pass: %v", err)
	}
	err = c.CreateRepo(ctx, Repo{Name: "x"})
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("POST should be refused, got %v", err)
	}
	if err := c.UpdatePublish(ctx, "", "p", "d", Signing{}, false); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("publish update should be refused, got %v", err)
	}
	if hits != 1 {
		t.Fatalf("server saw %d requests, want 1", hits)
	}
}

func TestRequestPathsAndErrors(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/api/publish/filesystem:public:my__repo/noble/sources" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"unable to show: No source changes exist"}`))
			return
		}
		if r.URL.Path == "/api/repos/bad/file/up" {
			w.Write([]byte(`{"FailedFiles":["/x/up/a_1_arm64.deb"],"Report":{"Warnings":["package already exists and is different"]}}`))
			return
		}
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"unable to drop, local repo is published"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL+"/", Options{Token: "t0k"})
	ctx := context.Background()

	pending, err := c.PendingSources(ctx, "filesystem:public", "my_repo", "noble")
	if err != nil || pending != nil {
		t.Fatalf("no staged changes should be (nil, nil), got %v, %v", pending, err)
	}
	if gotAuth != "Bearer t0k" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if _, err := c.ImportDir(ctx, "bad", "up", false); err == nil {
		t.Fatal("FailedFiles must be an error")
	}
	if gotPath != "/api/repos/bad/file/up" {
		t.Fatalf("path = %q", gotPath)
	}
	err = c.DropRepo(ctx, "r")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict || ae.Message != "unable to drop, local repo is published" {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestIsLoopback(t *testing.T) {
	for url, want := range map[string]bool{
		"http://127.0.0.1:8080": true, "http://localhost:1": true, "http://[::1]:1": true,
		"http://10.0.0.8:8080": false, "https://aptly.example.com": false,
	} {
		c, err := New(url, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.IsLoopback(); got != want {
			t.Errorf("IsLoopback(%s) = %v", url, got)
		}
	}
}
