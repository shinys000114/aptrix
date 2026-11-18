// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// Package integration runs aptrix against a throwaway local aptly.
//
//	APTRIX_TEST_APTLY=/path/to/aptly go test -tags integration ./integration/
//
// It needs aptly >= 1.6 and dpkg-deb. It only ever talks to 127.0.0.1.
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shinys000114/aptrix/internal/aptly"
	"github.com/shinys000114/aptrix/internal/cli"
)

type harness struct {
	t   *testing.T
	dir string
	pub string
	url string
	cfg string
}

func start(t *testing.T) *harness { return startWith(t, "") }

// startWith starts aptly; a non-empty gnupgHome enables signing with it.
func startWith(t *testing.T, gnupgHome string) *harness {
	t.Helper()
	bin := os.Getenv("APTRIX_TEST_APTLY")
	if bin == "" {
		t.Skip("APTRIX_TEST_APTLY not set")
	}
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb not installed")
	}
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, pub: filepath.Join(dir, "pub")}
	sign := gnupgHome == ""
	conf := fmt.Sprintf(`{"rootDir":%q,"architectures":["arm64","amd64"],"gpgDisableSign":%t,"gpgDisableVerify":true,"gpgProvider":"gpg",
 "FileSystemPublishEndpoints":{"test":{"rootDir":%q,"linkMethod":"copy"}}}`, filepath.Join(dir, "root"), sign, h.pub)
	confPath := filepath.Join(dir, "aptly.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	h.url = "http://" + addr
	cmd := exec.Command(bin, "api", "serve", "-config="+confPath, "-listen="+addr)
	if gnupgHome != "" {
		cmd.Env = append(os.Environ(), "GNUPGHOME="+gnupgHome)
	}
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("aptly log:\n%s", log.String())
		}
	})
	c, _ := aptly.New(h.url, aptly.Options{})
	if !c.IsLoopback() {
		t.Fatal("refusing to test against a non-loopback aptly")
	}
	for i := 0; ; i++ {
		v, err := c.Version(context.Background())
		if err == nil {
			if !strings.HasPrefix(v, "1.6") && !strings.HasPrefix(v, "1.7") {
				t.Skipf("aptly %s is too old (need 1.6+)", v)
			}
			break
		}
		if i > 50 {
			t.Fatalf("aptly did not start: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return h
}

func (h *harness) config(body string) {
	h.configSigned("skip = true", body)
}

func (h *harness) configSigned(signing, body string) {
	h.t.Helper()
	h.cfg = filepath.Join(h.dir, "aptrix.toml")
	full := fmt.Sprintf("[aptly]\nurl = %q\n\n[signing]\n%s\n\n%s", h.url, signing, body)
	if err := os.WriteFile(h.cfg, []byte(full), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// run runs aptrix and returns stdout+stderr.
func (h *harness) run(args ...string) (string, error) {
	var out bytes.Buffer
	err := cli.RunWith(context.Background(), append([]string{"-c", h.cfg}, args...), cli.Streams{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out})
	return out.String(), err
}

func (h *harness) ok(args ...string) string {
	h.t.Helper()
	out, err := h.run(args...)
	if err != nil {
		h.t.Fatalf("aptrix %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (h *harness) fail(want string, args ...string) {
	h.t.Helper()
	out, err := h.run(args...)
	if err == nil {
		h.t.Fatalf("aptrix %s: expected failure\n%s", strings.Join(args, " "), out)
	}
	if !strings.Contains(err.Error()+out, want) {
		h.t.Fatalf("aptrix %s: error %q / output %q does not mention %q", strings.Join(args, " "), err, out, want)
	}
}

func (h *harness) deb(name, version, arch, marker string) string {
	h.t.Helper()
	root := filepath.Join(h.dir, "build", name+"-"+version+"-"+arch+"-"+marker)
	os.MkdirAll(filepath.Join(root, "DEBIAN"), 0o755)
	os.MkdirAll(filepath.Join(root, "usr/share", name), 0o755)
	ctl := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: t <t@example.invalid>\nDescription: test\n", name, version, arch)
	os.WriteFile(filepath.Join(root, "DEBIAN/control"), []byte(ctl), 0o644)
	os.WriteFile(filepath.Join(root, "usr/share", name, "marker"), []byte(marker), 0o644)
	outDir := filepath.Join(h.dir, "debs", marker)
	os.MkdirAll(outDir, 0o755)
	out := filepath.Join(outDir, fmt.Sprintf("%s_%s_%s.deb", name, version, arch))
	if b, err := exec.Command("dpkg-deb", "-Zxz", "--build", root, out).CombinedOutput(); err != nil {
		h.t.Fatalf("dpkg-deb: %v\n%s", err, b)
	}
	return out
}

func (h *harness) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(h.pub, rel))
	return string(b)
}

func (h *harness) components(dist string) string {
	for _, l := range strings.Split(h.read("t/dists/"+dist+"/Release"), "\n") {
		if strings.HasPrefix(l, "Components:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "Components:"))
		}
	}
	return ""
}

func (h *harness) packages(dist, comp string) string {
	return h.read("t/dists/" + dist + "/" + comp + "/binary-arm64/Packages")
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("expected %q in:\n%s", want, got)
	}
}

const baseConfig = `
[publish.t]
storage = "filesystem:test"
prefix = "t"
dists = ["jammy", "noble"]
components = ["main", "extra"]
architectures = ["arm64"]
origin = "test"
label = "Test"
comment = "t {dist}/{component}"

[publish.t.only]
extra = ["noble"]
`

func TestMatrixLifecycle(t *testing.T) {
	h := start(t)
	h.config(baseConfig)

	// 1. empty aptly -> repos and publishes are created
	out := h.ok("plan")
	mustContain(t, out, "+ repo     t-noble-extra")
	mustContain(t, out, "+ publish  filesystem:test:t jammy [main]")
	h.fail("--yes", "sync") // no TTY, no --yes
	h.ok("sync", "--yes")
	mustContain(t, h.ok("plan"), "No changes.")
	if got := h.components("noble"); got != "extra main" {
		t.Fatalf("noble components = %q", got)
	}
	if got := h.components("jammy"); got != "main" {
		t.Fatalf("jammy components = %q", got)
	}

	// 2. add with dist detection, multi-dist add, and conflicts
	h.ok("add", "t", "main", h.deb("hello", "1.0~noble", "arm64", "a"), h.deb("hello", "1.1~noble", "arm64", "a"), h.deb("hello", "1.2~noble", "arm64", "a"))
	h.ok("add", "t", "main", "--dist", "all", h.deb("tool", "2.0", "all", "a"))
	mustContain(t, h.packages("noble", "main"), "Version: 1.2~noble")
	mustContain(t, h.packages("jammy", "main"), "Package: tool")
	h.fail("cannot tell the dist", "add", "t", "main", h.deb("nodist", "1.0", "arm64", "a"))
	h.fail("not a cell", "add", "t", "extra", h.deb("x", "1.0~jammy", "arm64", "a"))
	h.fail("architecture amd64", "add", "t", "main", h.deb("y", "1.0~noble", "amd64", "a"))
	h.fail("could not add", "add", "t", "main", h.deb("hello", "1.0~noble", "arm64", "b"))
	h.ok("add", "t", "main", "--force-replace", h.deb("hello", "1.0~noble", "arm64", "b"))

	out = h.ok("ls", "t", "main", "--dist", "noble", "--name", "hello")
	mustContain(t, out, "3 package(s)")
	if strings.Index(out, "1.2~noble") > strings.Index(out, "1.0~noble") {
		t.Fatalf("ls should list newest first:\n%s", out)
	}

	// 3. rm --keep keeps the newest versions
	h.fail("--yes", "rm", "t", "main", "hello", "--keep", "1")
	h.ok("rm", "t", "main", "hello", "--keep", "1", "--yes")
	pk := h.packages("noble", "main")
	mustContain(t, pk, "Version: 1.2~noble")
	if strings.Contains(pk, "Version: 1.1~noble") || strings.Contains(pk, "Version: 1.0~noble") {
		t.Fatalf("old versions still published:\n%s", pk)
	}
	h.ok("rm", "t", "main", "tool=2.0", "--dist", "jammy", "--yes")
	if strings.Contains(h.packages("jammy", "main"), "Package: tool") {
		t.Fatal("tool still in jammy")
	}
	mustContain(t, h.packages("noble", "main"), "Package: tool")

	// 4. new component on every dist
	h.config(strings.Replace(baseConfig, `components = ["main", "extra"]`, `components = ["main", "extra", "new"]`, 1))
	out = h.ok("sync", "--yes")
	mustContain(t, out, "+ source   filesystem:test:t jammy new <- t-jammy-new")
	if got := h.components("jammy"); got != "main new" {
		t.Fatalf("jammy components = %q", got)
	}

	// 5. pruning: warn without --prune, block non-empty repos without --force
	h.ok("add", "t", "extra", h.deb("ext", "1.0~noble", "arm64", "a"))
	h.config(strings.Replace(baseConfig, "\n[publish.t.only]\nextra = [\"noble\"]\n", "", 1)) // drops "new"; extra everywhere
	h.config(`
[publish.t]
storage = "filesystem:test"
prefix = "t"
dists = ["jammy", "noble"]
components = ["main"]
architectures = ["arm64"]
origin = "test"
label = "Test"
`)
	out = h.ok("plan")
	mustContain(t, out, "not in the config (use --prune")
	mustContain(t, out, "No changes.")
	out = h.ok("plan", "--prune")
	mustContain(t, out, "- repo     t-jammy-new (0 packages)")
	mustContain(t, out, "blocked: filesystem:test:t noble component extra (repo t-noble-extra) still has 1 packages")
	if strings.Contains(out, "noble extra") && strings.Contains(out, "- source   filesystem:test:t noble extra") {
		t.Fatalf("non-empty component must not be unpublished without --force:\n%s", out)
	}
	h.ok("sync", "--prune", "--yes")
	if got := h.components("noble"); got != "extra main" {
		t.Fatalf("noble components after prune without force = %q", got)
	}
	mustContain(t, h.packages("noble", "extra"), "Package: ext")
	out = h.ok("plan", "--prune", "--force")
	mustContain(t, out, "- source   filesystem:test:t noble extra (repo t-noble-extra)")
	mustContain(t, out, "- repo     t-noble-extra (1 packages)")
	h.ok("sync", "--prune", "--force", "--yes")
	if got := h.components("noble"); got != "main" {
		t.Fatalf("noble components after forced prune = %q", got)
	}
	mustContain(t, h.ok("plan", "--prune", "--force"), "No changes.")
	c, _ := aptly.New(h.url, aptly.Options{})
	repos, _ := c.Repos(context.Background())
	for _, r := range repos {
		if r.Name == "t-jammy-new" || r.Name == "t-noble-extra" {
			t.Fatalf("%s should have been dropped", r.Name)
		}
	}

	// 6. dropping a dist
	h.config(`
[publish.t]
storage = "filesystem:test"
prefix = "t"
dists = ["noble"]
components = ["main"]
architectures = ["arm64"]
origin = "test"
label = "Test"
`)
	out = h.ok("plan", "--prune")
	mustContain(t, out, "- publish  filesystem:test:t jammy")
	h.ok("sync", "--prune", "--yes")
	if h.read("t/dists/jammy/Release") != "" {
		t.Fatal("jammy still published")
	}

	// 7. staged changes from someone else block sync
	h.config(`
[publish.t]
storage = "filesystem:test"
prefix = "t"
dists = ["noble"]
components = ["main", "late"]
architectures = ["arm64"]
origin = "test"
label = "Test"
`)
	ctx := context.Background()
	c.CreateRepo(ctx, aptly.Repo{Name: "stray"})
	if err := c.StageAddSource(ctx, "filesystem:test", "t", "noble", aptly.Source{Component: "stray", Name: "stray"}); err != nil {
		t.Fatal(err)
	}
	h.fail("--discard-staged", "sync", "--yes")
	h.ok("sync", "--yes", "--discard-staged")
	if got := h.components("noble"); got != "late main" {
		t.Fatalf("noble components = %q", got)
	}
}

// TestSignedPublish covers the production path: GpgKey + PassphraseFile.
func TestSignedPublish(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed")
	}
	home, err := os.MkdirTemp("", "aptrix-gpg-") // short path: gpg-agent socket length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run()
		os.RemoveAll(home)
	})
	os.Chmod(home, 0o700)
	pass := filepath.Join(home, "passphrase")
	os.WriteFile(pass, []byte("s3cret-test\n"), 0o600)
	gpg := func(args ...string) string {
		cmd := exec.Command("gpg", append([]string{"--homedir", home, "--batch"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gpg %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	gpg("--pinentry-mode", "loopback", "--passphrase-file", pass, "--quick-gen-key", "aptrix test <t@example.invalid>", "ed25519", "sign", "never")
	var fpr string
	for _, l := range strings.Split(gpg("--with-colons", "--list-secret-keys"), "\n") {
		if strings.HasPrefix(l, "fpr:") {
			fpr = strings.Split(l, ":")[9]
			break
		}
	}
	if fpr == "" {
		t.Fatal("no fingerprint")
	}

	h := startWith(t, home)
	h.configSigned(fmt.Sprintf("gpg_key = %q\npassphrase_file = %q", fpr, pass), `
[publish.t]
storage = "filesystem:test"
prefix = "t"
dists = ["noble"]
components = ["main"]
architectures = ["arm64"]
`)
	h.ok("sync", "--yes")
	h.ok("add", "t", "main", h.deb("hello", "1.0~noble", "arm64", "a"))
	inrel := filepath.Join(h.pub, "t/dists/noble/InRelease")
	out := gpg("--verify", inrel)
	mustContain(t, out, "Good signature")
	mustContain(t, h.read("t/dists/noble/InRelease"), "Package") // Release lists binary-arm64/Packages
	if _, err := os.Stat(filepath.Join(h.pub, "t/dists/noble/Release.gpg")); err != nil {
		t.Fatal("Release.gpg missing")
	}
}

// TestRootPrefix publishes at the default publish root with no storage/prefix.
func TestRootPrefix(t *testing.T) {
	h := start(t)
	h.config(`
[publish.root]
dists = ["noble"]
components = ["main"]
architectures = ["arm64"]
`)
	h.ok("sync", "--yes")
	mustContain(t, h.ok("plan"), "No changes.")
	h.ok("add", "root", "main", h.deb("hello", "1.0~noble", "arm64", "a"))
	b, err := os.ReadFile(filepath.Join(h.dir, "root", "public", "dists", "noble", "main", "binary-arm64", "Packages"))
	if err != nil || !strings.Contains(string(b), "Package: hello") {
		t.Fatalf("root publish missing package: %v\n%s", err, b)
	}
}

func labelFrom(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "snapshot "+prefix) {
			return strings.Fields(l)[1]
		}
	}
	t.Fatalf("no %s snapshot label in:\n%s", prefix, out)
	return ""
}

func TestSnapshotRollback(t *testing.T) {
	h := start(t)
	h.config(`
[publish.t]
storage = "filesystem:test"
prefix = "t"
dists = ["noble"]
components = ["main", "extra"]
architectures = ["arm64"]
`)
	h.ok("sync", "--yes")
	h.ok("add", "t", "main",
		h.deb("hello", "1.0~noble", "arm64", "a"), h.deb("hello", "1.1~noble", "arm64", "a"),
		h.deb("hello-dbg", "1.0~noble", "arm64", "a"), h.deb("hello-dbg", "1.1~noble", "arm64", "a"),
		h.deb("tool", "1.0~noble", "arm64", "a"))
	h.ok("add", "t", "extra", h.deb("x-dbg", "1.0~noble", "arm64", "a"))

	// glob rm only touches the named component and snapshots it first
	h.fail("--yes", "rm", "t", "main", "*-dbg")
	out := h.ok("rm", "t", "main", "*-dbg", "--yes")
	label := labelFrom(t, out, "pre-rm-")
	mustContain(t, out, "removed 2 from t-noble-main")
	pk := h.packages("noble", "main")
	if strings.Contains(pk, "Package: hello-dbg") {
		t.Fatalf("dbg still published:\n%s", pk)
	}
	mustContain(t, pk, "Package: tool")
	mustContain(t, h.packages("noble", "extra"), "Package: x-dbg")
	mustContain(t, h.ok("snapshot", "list", "t"), label)

	// rollback restores exactly, and is itself undoable
	h.fail("--yes", "rollback", "t", label)
	out = h.ok("rollback", "t", label, "--yes")
	mustContain(t, out, "restored t-noble-main (+2 -0)")
	undo := labelFrom(t, out, "pre-rollback-")
	mustContain(t, h.packages("noble", "main"), "Version: 1.1~noble\n")
	if n := strings.Count(h.packages("noble", "main"), "Package: hello-dbg"); n != 2 {
		t.Fatalf("hello-dbg versions after rollback = %d, want 2", n)
	}
	mustContain(t, h.ok("rollback", "t", label, "--yes"), "Already at "+label)
	h.ok("rollback", "t", undo, "--yes", "--no-snapshot")
	if strings.Contains(h.packages("noble", "main"), "Package: hello-dbg") {
		t.Fatal("undoing the rollback should remove dbg again")
	}

	// --keep with a glob keeps N versions per package name
	h.ok("rollback", "t", label, "--yes", "--no-snapshot")
	h.ok("rm", "t", "main", "hello*", "--keep", "1", "--yes", "--no-snapshot")
	pk = h.packages("noble", "main")
	if strings.Count(pk, "Package: hello\n") != 1 || strings.Count(pk, "Package: hello-dbg\n") != 1 {
		t.Fatalf("keep 1 per name failed:\n%s", pk)
	}

	// manual snapshots
	h.ok("snapshot", "create", "t", "--label", "manual1")
	h.fail("already exists", "snapshot", "create", "t", "--label", "manual1")
	mustContain(t, h.ok("snapshot", "list", "t"), "manual1")
	h.ok("snapshot", "drop", "t", "manual1", "--yes")
	if strings.Contains(h.ok("snapshot", "list", "t"), "manual1") {
		t.Fatal("manual1 not dropped")
	}
	h.fail("no snapshot labelled", "rollback", "t", "manual1", "--yes")
}

const featureConfig = `
[publish.b]
storage = "filesystem:test"
prefix = "b"
dists = ["jammy", "noble"]
components = ["main", "testing"]
architectures = ["arm64"]
keep = 2
`

func (h *harness) packagesOf(prefix, dist, comp string) string {
	return h.read(prefix + "/dists/" + dist + "/" + comp + "/binary-arm64/Packages")
}

func TestRetentionPromoteFindURL(t *testing.T) {
	h := start(t)
	h.config(featureConfig)
	h.ok("sync", "--yes")

	// keep = 2: adding three versions leaves the newest two, after a snapshot
	out := h.ok("add", "b", "main", h.deb("kern", "1.0~noble", "arm64", "a"), h.deb("kern", "1.1~noble", "arm64", "a"), h.deb("kern", "1.2~noble", "arm64", "a"))
	mustContain(t, out, "snapshot pre-add-")
	mustContain(t, out, "removed 1 from b-noble-main (keep 2)")
	pk := h.packagesOf("b", "noble", "main")
	if strings.Contains(pk, "Version: 1.0~noble") || !strings.Contains(pk, "Version: 1.2~noble") {
		t.Fatalf("keep 2 not applied:\n%s", pk)
	}

	// add from URLs with a checksums file
	tool := h.deb("tool", "1.0", "all", "a")
	sum := sha256.Sum256(must(os.ReadFile(tool)))
	files := http.FileServer(http.Dir(filepath.Dir(tool)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums.txt":
			fmt.Fprintf(w, "%x  tool_1.0_all.deb\n", sum)
		case "/bad.txt":
			fmt.Fprintf(w, "%064x  tool_1.0_all.deb\n", 0)
		case "/other.txt":
			fmt.Fprintf(w, "%x  something_else.deb\n", sum)
		default:
			files.ServeHTTP(w, r)
		}
	}))
	defer srv.Close()
	h.fail("does not match", "add", "b", "main", "--dist", "all", srv.URL+"/tool_1.0_all.deb", "--checksums", srv.URL+"/bad.txt")
	h.fail("is not listed", "add", "b", "main", "--dist", "all", srv.URL+"/tool_1.0_all.deb", "--checksums", srv.URL+"/other.txt")
	mustContain(t, h.ok("add", "b", "main", "--dist", "all", srv.URL+"/tool_1.0_all.deb", "--checksums", srv.URL+"/checksums.txt"), "checksums ok (1 file(s))")
	mustContain(t, h.packagesOf("b", "jammy", "main"), "Package: tool")

	// find
	out = h.ok("find", "kern", "t*")
	mustContain(t, out, "4 package(s)") // k 1.2, k 1.1, tool jammy, tool noble
	mustContain(t, h.ok("find", "kern", "--newest"), "1 package(s)")
	var found []map[string]string
	if err := json.Unmarshal([]byte(h.ok("find", "kern", "--version", ">=1.2~noble", "--json")), &found); err != nil || len(found) != 1 || found[0]["version"] != "1.2~noble" {
		t.Fatalf("find --json: %v %v", found, err)
	}

	// manual snapshot for diff later
	h.ok("snapshot", "create", "b", "--label", "before-promote")

	// promote copies the newest version; a second run is a no-op
	out = h.ok("promote", "b/main", "b/testing", "kern", "--yes")
	mustContain(t, out, "added 1 to b-noble-testing")
	mustContain(t, h.packagesOf("b", "noble", "testing"), "Version: 1.2~noble")
	mustContain(t, h.ok("promote", "b/main", "b/testing", "kern", "--yes"), "Nothing to promote (1 already present)")
	// --to-dist
	h.ok("promote", "b/main", "b/testing", "tool", "--dist", "noble", "--to-dist", "jammy", "--yes")
	mustContain(t, h.packagesOf("b", "jammy", "testing"), "Package: tool")
	// --move back out of testing
	h.ok("promote", "b/testing", "b/main", "kern", "--move", "--yes")
	if strings.Contains(h.packagesOf("b", "noble", "testing"), "Package: kern\n") {
		t.Fatal("move left k in testing")
	}
	mustContain(t, h.packagesOf("b", "noble", "main"), "Version: 1.2~noble")

	// prune-versions
	out = h.ok("prune-versions", "b", "main", "--keep", "1", "--yes")
	mustContain(t, out, "removed 1 from b-noble-main (keep 1)")
	if strings.Contains(h.packagesOf("b", "noble", "main"), "Version: 1.1~noble") {
		t.Fatal("prune-versions kept 1.1")
	}
	mustContain(t, h.ok("prune-versions", "b", "--keep", "1", "--yes"), "Nothing to prune")

	// snapshot diff: label -> now, and label -> label
	out = h.ok("snapshot", "diff", "b", "before-promote")
	mustContain(t, out, "before-promote -> now")
	mustContain(t, out, "- noble  main     kern  1.1~noble")
	mustContain(t, out, "+ jammy  testing  tool  1.0")
	h.ok("snapshot", "create", "b", "--label", "after")
	var diff []map[string]string
	if err := json.Unmarshal([]byte(h.ok("snapshot", "diff", "b", "before-promote", "after", "--json")), &diff); err != nil || len(diff) != 2 { // kern copied to testing then moved back: net zero
		t.Fatalf("diff --json: %v %v", diff, err)
	}
	mustContain(t, h.ok("snapshot", "diff", "b", "after"), "No differences.")

	// JSON outputs parse
	var status []map[string]any
	if err := json.Unmarshal([]byte(h.ok("status", "--json")), &status); err != nil || len(status) != 4 {
		t.Fatalf("status --json: %v %v", status, err)
	}
	var planOut map[string]any
	if err := json.Unmarshal([]byte(h.ok("plan", "--json")), &planOut); err != nil || len(planOut["steps"].([]any)) != 0 {
		t.Fatalf("plan --json: %v %v", planOut, err)
	}
	var labelsOut []map[string]any
	if err := json.Unmarshal([]byte(h.ok("snapshot", "list", "b", "--json")), &labelsOut); err != nil || len(labelsOut) < 3 {
		t.Fatalf("snapshot list --json: %v %v", labelsOut, err)
	}

	// snapshot prune: automatic labels only, unless --include-manual
	h.ok("snapshot", "prune", "b", "--older-than", "0s", "--yes")
	list := h.ok("snapshot", "list", "b")
	if strings.Contains(list, "pre-") || !strings.Contains(list, "before-promote") {
		t.Fatalf("prune should drop only automatic labels:\n%s", list)
	}
	h.ok("snapshot", "prune", "b", "--older-than", "0s", "--include-manual", "--yes")
	mustContain(t, h.ok("snapshot", "list", "b"), "0 snapshot label(s)")

	// gc
	mustContain(t, h.ok("gc", "--yes"), "done")
	h.fail("--yes", "gc")
}

func TestSnapshotExpiry(t *testing.T) {
	h := start(t)
	h.config(strings.Replace(featureConfig, "keep = 2", `snapshot_max_age = "1s"`, 1))
	h.ok("sync", "--yes")
	h.ok("add", "b", "main", h.deb("kern", "1.0~noble", "arm64", "a"), h.deb("kern", "1.1~noble", "arm64", "a"))
	first := labelFrom(t, h.ok("rm", "b", "main", "kern=1.0~noble", "--yes"), "pre-rm-")
	time.Sleep(2 * time.Second)
	out := h.ok("rm", "b", "main", "kern=1.1~noble", "--yes")
	mustContain(t, out, "expired snapshot "+first)
	second := labelFrom(t, out, "pre-rm-")
	list := h.ok("snapshot", "list", "b")
	if strings.Contains(list, first) || !strings.Contains(list, second) {
		t.Fatalf("expiry kept the wrong labels:\n%s", list)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestCrossPublishPromote(t *testing.T) {
	h := start(t)
	h.config(featureConfig + `
[publish.pub]
storage = "filesystem:test"
prefix = "pub"
dists = ["noble"]
components = ["main"]
architectures = ["amd64"]
`)
	h.ok("sync", "--yes")
	h.ok("add", "b", "main", h.deb("kern", "1.0~noble", "arm64", "a"))
	h.ok("add", "b", "main", "--dist", "all", h.deb("tool", "1.0", "all", "a"))

	// destination does not publish arm64
	h.fail("architecture arm64 is not published by pub", "promote", "b/main", "pub/main", "kern", "--yes")

	// same repo as source: refused, nothing deleted
	h.fail("destination is the source repo", "promote", "b/main", "b/main", "tool", "--dist", "noble", "--to-dist", "noble", "--move", "--yes")
	mustContain(t, h.packagesOf("b", "noble", "main"), "Package: tool")

	// cross-publish move: only the dist both sides have (noble)
	out := h.ok("promote", "b/main", "pub/main", "tool", "--move", "--yes")
	mustContain(t, out, "added 1 to pub-noble-main")
	mustContain(t, out, "removed 1 from b-noble-main (moved)")
	if strings.Count(out, "snapshot pre-promote-") != 2 {
		t.Fatalf("expected one snapshot per publish:\n%s", out)
	}
	mustContain(t, h.read("pub/dists/noble/main/binary-amd64/Packages"), "Package: tool")
	if strings.Contains(h.packagesOf("b", "noble", "main"), "Package: tool") {
		t.Fatal("tool still published in b noble after move")
	}
	mustContain(t, h.packagesOf("b", "jammy", "main"), "Package: tool") // jammy untouched
}

func TestGCKeepsSnapshots(t *testing.T) {
	h := start(t)
	h.config(featureConfig)
	h.ok("sync", "--yes")
	h.ok("add", "b", "main", h.deb("kern", "1.0~noble", "arm64", "a"), h.deb("kern-dbg", "1.0~noble", "arm64", "a"))
	label := labelFrom(t, h.ok("rm", "b", "main", "*-dbg", "--yes"), "pre-rm-")
	h.ok("gc", "--yes")
	// the snapshot still pins the package, so rollback works after gc
	mustContain(t, h.ok("rollback", "b", label, "--yes"), "restored b-noble-main (+1 -0)")
	pk := h.packagesOf("b", "noble", "main")
	mustContain(t, pk, "Package: kern-dbg")
	var fn string
	for _, l := range strings.Split(pk, "\n") {
		if strings.HasPrefix(l, "Filename: ") && strings.Contains(l, "kern-dbg") {
			fn = strings.TrimPrefix(l, "Filename: ")
		}
	}
	if _, err := os.Stat(filepath.Join(h.pub, "b", fn)); err != nil {
		t.Fatalf("restored package file missing from the published pool: %v", err)
	}

	// without a snapshot, gc really frees it and rollback cannot help
	h.ok("rm", "b", "main", "*-dbg", "--yes", "--no-snapshot")
	h.ok("snapshot", "prune", "b", "--older-than", "0s", "--yes")
	h.ok("gc", "--yes")
	h.fail("no snapshot labelled", "rollback", "b", label, "--yes")
}

func TestRetireAndMerge(t *testing.T) {
	h := start(t)
	merged := `
[publish.new]
storage = "filesystem:test"
prefix = "new"
dists = ["jammy", "noble"]
components = ["main"]
architectures = ["arm64"]
`
	old := `
[publish.old]
storage = "filesystem:test"
prefix = "old"
repo_name = "old-{dist}"
dists = ["jammy", "noble"]
components = ["old"]
architectures = ["arm64"]
`
	h.config(old + merged)
	h.ok("sync", "--yes")
	h.ok("add", "old", "old", "--dist", "all", h.deb("tool", "1.0", "all", "a"), h.deb("tool", "1.1", "all", "a"))

	// merge: copy everything, then move what is left
	h.ok("promote", "old/old", "new/main", "*", "--all-versions", "--yes")
	if n := strings.Count(h.packagesOf("new", "noble", "main"), "Package: tool"); n != 2 {
		t.Fatalf("new noble has %d tool versions", n)
	}
	h.ok("rm", "old", "old", "*", "--yes", "--no-snapshot")
	h.ok("snapshot", "prune", "old", "--older-than", "0s", "--include-manual", "--yes")

	// retire the old publish
	h.config(strings.Replace(old, "repo_name", "retire = true\nrepo_name", 1) + merged)
	out := h.ok("plan")
	mustContain(t, out, "publish.old is retired")
	mustContain(t, out, "No changes.")
	h.fail("is retired", "add", "old", "old", "--dist", "noble", h.deb("x", "1.0", "all", "a"))
	h.fail("is retired", "promote", "new/main", "old/old", "tool", "--yes")
	mustContain(t, h.ok("status", "old"), "(retired")
	out = h.ok("plan", "--prune")
	mustContain(t, out, "- publish  filesystem:test:old jammy")
	mustContain(t, out, "- repo     old-noble (0 packages)")
	if strings.Contains(out, "new") {
		t.Fatalf("retiring old must not touch new:\n%s", out)
	}
	h.ok("sync", "--prune", "--yes")
	if h.read("old/dists/noble/Release") != "" {
		t.Fatal("old still published")
	}
	mustContain(t, h.packagesOf("new", "noble", "main"), "Package: tool")

	// finally drop the table: nothing left to do
	h.config(merged)
	mustContain(t, h.ok("plan", "--prune"), "No changes.")
	c, _ := aptly.New(h.url, aptly.Options{})
	repos, _ := c.Repos(context.Background())
	for _, r := range repos {
		if strings.HasPrefix(r.Name, "old-") {
			t.Fatalf("leftover repo %s", r.Name)
		}
	}
}
