package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahulbalajee/blogctl/internal/blogtest"
	"github.com/rahulbalajee/blogctl/internal/cli"
	"github.com/rahulbalajee/blogctl/internal/client"
)

type result struct {
	code   int
	stdout string
	stderr string
}

// run executes blogctl against srv with credentials from the environment
// unless env overrides them.
func run(t *testing.T, srv *blogtest.Server, env map[string]string, stdin string, args ...string) result {
	t.Helper()
	vars := map[string]string{
		"BLOGCTL_BASE_URL":      srv.URL,
		"BLOGCTL_CLIENT_ID":     blogtest.ClientID,
		"BLOGCTL_CLIENT_SECRET": blogtest.ClientSecret,
	}
	for k, v := range env {
		if v == "" {
			delete(vars, k)
		} else {
			vars[k] = v
		}
	}
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), cli.Env{
		Args:    args,
		Stdin:   strings.NewReader(stdin),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Getenv:  func(name string) string { return vars[name] },
		Version: "test",
		HomeDir: func() (string, error) { return home, nil },
		Sleep:   func(context.Context, time.Duration) error { return nil },
	})
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const articleJSON = `{"title": "Infrastructure notes", "summary": "Notes prepared for human review.", "body": "<h2>Overview</h2><p>A draft.</p>", "tags": ["infrastructure"]}`

func TestCreateGetEditUpdateLoop(t *testing.T) {
	srv := blogtest.New(t)
	created := run(t, srv, nil, "", "posts", "create", "infrastructure-notes", "--file", writeFile(t, "article.json", articleJSON))
	if created.code != cli.ExitOK || !strings.Contains(created.stdout, "Created post infrastructure-notes at revision 1.") {
		t.Fatalf("create = %+v", created)
	}

	edit := filepath.Join(t.TempDir(), "edit.json")
	got := run(t, srv, nil, "", "posts", "get", "infrastructure-notes", "--edit-file", edit)
	if got.code != cli.ExitOK || !strings.Contains(got.stdout, "Revision:") || !strings.Contains(got.stdout, "<h2>Overview</h2>") {
		t.Fatalf("get = %+v", got)
	}
	var doc struct {
		Revision int64          `json:"revision"`
		Article  client.Article `json:"article"`
	}
	data, err := os.ReadFile(edit)
	if err != nil || json.Unmarshal(data, &doc) != nil || doc.Revision != 1 {
		t.Fatalf("edit file = %s, %v", data, err)
	}
	doc.Article.Title = "Infrastructure notes, revised"
	data, _ = json.Marshal(doc)
	if err := os.WriteFile(edit, data, 0o600); err != nil {
		t.Fatal(err)
	}

	updated := run(t, srv, nil, "", "--output", "json", "posts", "update", "infrastructure-notes", "--file", edit)
	var post struct {
		client.Post
		Reconciled bool `json:"reconciled"`
	}
	if updated.code != cli.ExitOK || json.Unmarshal([]byte(updated.stdout), &post) != nil || post.Revision != 2 || post.Draft.Title != "Infrastructure notes, revised" {
		t.Fatalf("update = %+v", updated)
	}
	// The same edit file is now stale: it must conflict, not overwrite.
	stale := run(t, srv, nil, "", "--output", "json", "posts", "update", "infrastructure-notes", "--file", edit)
	if stale.code != cli.ExitConflict || stale.stdout != "" || !strings.Contains(stale.stderr, `"currentRevision":2`) {
		t.Fatalf("stale update = %+v", stale)
	}

	list := run(t, srv, nil, "", "posts", "list")
	if list.code != cli.ExitOK || !strings.Contains(list.stdout, "infrastructure-notes") {
		t.Fatalf("list = %+v", list)
	}
}

func TestCreateFromStdin(t *testing.T) {
	srv := blogtest.New(t)
	out := run(t, srv, nil, articleJSON, "posts", "create", "from-stdin", "-f", "-")
	if out.code != cli.ExitOK {
		t.Fatalf("stdin create = %+v", out)
	}
}

func jsonError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func TestExitCodes(t *testing.T) {
	article := writeFile(t, "article.json", articleJSON)
	for _, tc := range []struct {
		name     string
		setup    func(*blogtest.Server)
		env      map[string]string
		args     []string
		want     int
		wantKind string
	}{
		{name: "blocked at the edge", setup: func(s *blogtest.Server) { s.SetJWKSBlocked(true) }, args: []string{"auth", "check"}, want: cli.ExitBlocked, wantKind: "blocked"},
		{name: "not found", args: []string{"posts", "get", "missing"}, want: cli.ExitNotFound, wantKind: "not_found"},
		{name: "credentials rejected", env: map[string]string{"BLOGCTL_CLIENT_SECRET": strings.Repeat("9", 64)}, args: []string{"posts", "list"}, want: cli.ExitAuth, wantKind: "auth"},
		{name: "no credentials", env: map[string]string{"BLOGCTL_CLIENT_ID": "", "BLOGCTL_CLIENT_SECRET": ""}, args: []string{"posts", "list"}, want: cli.ExitAuth, wantKind: "auth"},
		{name: "forbidden", setup: func(s *blogtest.Server) {
			s.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
				if r.URL.Path == "/api/v1/posts" && r.Method == http.MethodPost {
					jsonError(w, http.StatusForbidden, "insufficient_scope")
					return true
				}
				return false
			})
		}, args: []string{"posts", "create", "notes", "--file", article}, want: cli.ExitForbidden, wantKind: "forbidden"},
		{name: "rate limited", setup: func(s *blogtest.Server) {
			s.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
				if strings.HasPrefix(r.URL.Path, "/api/v1/") {
					w.Header().Set("Retry-After", "120")
					jsonError(w, http.StatusTooManyRequests, "rate_limited")
					return true
				}
				return false
			})
		}, args: []string{"posts", "list"}, want: cli.ExitRateLimited, wantKind: "rate_limited"},
		{name: "unavailable", setup: func(s *blogtest.Server) {
			s.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
				if strings.HasPrefix(r.URL.Path, "/api/v1/") {
					jsonError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
					return true
				}
				return false
			})
		}, args: []string{"posts", "list"}, want: cli.ExitUnavailable, wantKind: "unavailable"},
		{name: "invalid slug", args: []string{"posts", "get", "Not A Slug"}, want: cli.ExitUsage, wantKind: "usage"},
		{name: "unknown command", args: []string{"posts", "publish", "notes"}, want: cli.ExitUsage, wantKind: "usage"},
		{name: "missing file flag", args: []string{"posts", "create", "notes"}, want: cli.ExitUsage, wantKind: "usage"},
		{name: "extra argument", args: []string{"posts", "get", "a", "b"}, want: cli.ExitUsage, wantKind: "usage"},
		{name: "null in article", args: []string{"posts", "create", "notes", "--file", writeFile(t, "null.json", `{"title": null}`)}, want: cli.ExitUsage, wantKind: "usage"},
		{name: "unknown field", args: []string{"posts", "create", "notes", "--file", writeFile(t, "extra.json", `{"title": "Notes", "publish": true}`)}, want: cli.ExitUsage, wantKind: "usage"},
		{name: "article too short", args: []string{"posts", "create", "notes", "--file", writeFile(t, "short.json", `{"title": "No", "summary": "s", "body": "<p>x</p>"}`)}, want: cli.ExitUsage, wantKind: "invalid"},
		{name: "bad output flag", args: []string{"--output", "yaml", "version"}, want: cli.ExitUsage, wantKind: "usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := blogtest.New(t)
			if tc.setup != nil {
				tc.setup(srv)
			}
			text := run(t, srv, tc.env, "", tc.args...)
			if text.code != tc.want || text.stderr == "" {
				t.Fatalf("text: %+v, want exit %d", text, tc.want)
			}
			if tc.name == "bad output flag" {
				return
			}
			jsonOut := run(t, srv, tc.env, "", append([]string{"--output", "json"}, tc.args...)...)
			var body struct {
				Error struct {
					Kind    string `json:"kind"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if jsonOut.code != tc.want || jsonOut.stdout != "" || json.Unmarshal([]byte(jsonOut.stderr), &body) != nil || body.Error.Kind != tc.wantKind || body.Error.Message == "" {
				t.Fatalf("json: %+v, want exit %d and kind %q", jsonOut, tc.want, tc.wantKind)
			}
		})
	}
}

func TestCredentialsFile(t *testing.T) {
	srv := blogtest.New(t)
	creds := `{"clientId": "` + blogtest.ClientID + `", "clientSecret": "` + blogtest.ClientSecret + `"}`
	path := writeFile(t, "credentials.json", creds)
	noEnv := map[string]string{"BLOGCTL_CLIENT_ID": "", "BLOGCTL_CLIENT_SECRET": "", "BLOGCTL_CREDENTIALS_FILE": path}
	if out := run(t, srv, noEnv, "", "auth", "check"); out.code != cli.ExitOK || !strings.Contains(out.stdout, "Credentials OK") {
		t.Fatalf("private file = %+v", out)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if out := run(t, srv, noEnv, "", "auth", "check"); out.code != cli.ExitAuth || !strings.Contains(out.stderr, "chmod 600") {
		t.Fatalf("readable file = %+v", out)
	}
}

func TestCredentialsFromFileDescriptor(t *testing.T) {
	srv := blogtest.New(t)
	creds := `{"clientId": "` + blogtest.ClientID + `", "clientSecret": "` + blogtest.ClientSecret + `"}`
	var stdout, stderr bytes.Buffer
	opened := -1
	code := cli.Run(context.Background(), cli.Env{
		Args:   []string{"--credentials-fd", "3", "auth", "check"},
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(name string) string {
			if name == "BLOGCTL_BASE_URL" {
				return srv.URL
			}
			return ""
		},
		Version: "test",
		OpenFD: func(fd int) (io.ReadCloser, error) {
			opened = fd
			return io.NopCloser(strings.NewReader(creds)), nil
		},
	})
	if code != cli.ExitOK || opened != 3 {
		t.Fatalf("fd credentials: code %d, fd %d, %s", code, opened, stderr.String())
	}
}

func TestMediaUpload(t *testing.T) {
	srv := blogtest.New(t)
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chart.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out := run(t, srv, nil, "", "--output", "json", "media", "upload", "notes", path, "--alt", "A chart")
	var image client.UploadedImage
	if out.code != cli.ExitOK || json.Unmarshal([]byte(out.stdout), &image) != nil || !strings.HasPrefix(image.PreviewURL, "/api/admin/media/notes/") || !strings.Contains(image.HTML, `alt="A chart"`) {
		t.Fatalf("upload = %+v", out)
	}
	if missing := run(t, srv, nil, "", "media", "upload", "notes", path); missing.code != cli.ExitUsage {
		t.Fatalf("upload without --alt = %+v", missing)
	}
}

func TestVersion(t *testing.T) {
	srv := blogtest.New(t)
	if out := run(t, srv, nil, "", "version"); out.code != cli.ExitOK || out.stdout != "blogctl test\n" {
		t.Fatalf("version = %+v", out)
	}
	if out := run(t, srv, nil, "", "--version"); out.code != cli.ExitOK || out.stdout != "blogctl test\n" {
		t.Fatalf("--version = %+v", out)
	}
}

// No command, on success or failure and in either output mode, ever prints
// the secret, the client ID, the Basic credentials or an access token.
func TestNoCredentialsOrTokensInOutput(t *testing.T) {
	srv := blogtest.New(t)
	article := writeFile(t, "article.json", articleJSON)
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewGray(image.Rect(0, 0, 4, 4)))
	img := filepath.Join(t.TempDir(), "i.png")
	_ = os.WriteFile(img, buf.Bytes(), 0o600)
	commands := [][]string{
		{"auth", "check"},
		{"posts", "create", "leak-check", "--file", article},
		{"posts", "create", "leak-check", "--file", article},
		{"posts", "list", "--all"},
		{"posts", "get", "leak-check"},
		{"posts", "get", "missing"},
		{"media", "upload", "leak-check", img, "--alt", "Alt"},
		{"posts", "update", "leak-check", "--file", writeFile(t, "stale.json", `{"revision": 9, "article": `+articleJSON+`}`)},
	}
	basic := base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(blogtest.ClientID) + ":" + url.QueryEscape(blogtest.ClientSecret)))
	for _, args := range commands {
		for _, mode := range []string{"text", "json"} {
			out := run(t, srv, nil, "", append([]string{"--output", mode}, args...)...)
			srv.Lock()
			secrets := append([]string{blogtest.ClientSecret, blogtest.ClientID, basic, "signed-policy", "signature"}, srv.Tokens...)
			srv.Unlock()
			for _, secret := range secrets {
				if strings.Contains(out.stdout+out.stderr, secret) {
					t.Fatalf("%v (%s) printed a credential or upload field", args, mode)
				}
			}
		}
	}
	// The wrong-secret path must not echo the attempted secret either.
	wrong := strings.Repeat("9", 64)
	out := run(t, srv, map[string]string{"BLOGCTL_CLIENT_SECRET": wrong}, "", "posts", "list")
	if strings.Contains(out.stdout+out.stderr, wrong) {
		t.Fatal("rejected secret was printed")
	}
}

func TestUsageErrorsNeverExitZero(t *testing.T) {
	srv := blogtest.New(t)
	noCreds := map[string]string{"BLOGCTL_CLIENT_ID": "", "BLOGCTL_CLIENT_SECRET": ""}
	for _, args := range [][]string{
		{"help", "nonsense"},
		{"--base-url", "https://attacker.example", "posts", "list"},
		{"completion", "bash"},
		{"posts", "list", "--limit", "0"},
		{"--credentials-fd", "1", "posts", "list"},
		{"posts", "create", "dup", "--file", writeFile(t, "dup.json", `{"title": "Notes", "title": "Again", "summary": "A summary long enough.", "body": "<p>x</p>"}`)},
		{"posts", "create", "case", "--file", writeFile(t, "case.json", `{"Title": "Notes", "summary": "A summary long enough.", "body": "<p>x</p>"}`)},
		{"posts", "update", "nested", "--file", writeFile(t, "nested.json", `{"revision": 1, "article": {"title": "Notes", "Body": "<p>x</p>"}}`)},
	} {
		if out := run(t, srv, noCreds, "", args...); out.code != cli.ExitUsage {
			t.Errorf("%v: exit %d (%s), want %d", args, out.code, out.stderr, cli.ExitUsage)
		}
	}
	if out := run(t, srv, nil, "", "help", "posts"); out.code != cli.ExitOK || !strings.Contains(out.stdout, "posts") {
		t.Errorf("help posts = %+v", out)
	}
}

// A flag error before -o json is reached still produces a JSON error.
func TestEarlyFlagErrorsHonourJSONOutput(t *testing.T) {
	srv := blogtest.New(t)
	out := run(t, srv, nil, "", "--bogus", "-o", "json", "posts", "list")
	var body struct {
		Error struct {
			Kind string `json:"kind"`
		} `json:"error"`
	}
	if out.code != cli.ExitUsage || json.Unmarshal([]byte(out.stderr), &body) != nil || body.Error.Kind != "usage" {
		t.Fatalf("early flag error = %+v", out)
	}
}

func TestLargeEditFileRoundTrips(t *testing.T) {
	srv := blogtest.New(t)
	big := `{"title": "Markup heavy", "summary": "A summary long enough.", "body": "` + strings.Repeat("<p>&amp;</p>", 61400/12) + `"}`
	if out := run(t, srv, nil, "", "posts", "create", "markup", "--file", writeFile(t, "big.json", big)); out.code != cli.ExitOK {
		t.Fatalf("create = %+v", out)
	}
	edit := filepath.Join(t.TempDir(), "edit.json")
	if out := run(t, srv, nil, "", "posts", "get", "markup", "--edit-file", edit); out.code != cli.ExitOK {
		t.Fatalf("get = %+v", out)
	}
	if out := run(t, srv, nil, "", "posts", "update", "markup", "--file", edit); out.code != cli.ExitOK {
		t.Fatalf("update from blogctl's own edit file = %+v", out)
	}
}

// Ctrl-C ends a command waiting for stdin.
func TestInterruptStopsAStdinWait(t *testing.T) {
	srv := blogtest.New(t)
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, cli.Env{
			Args:   []string{"posts", "create", "notes", "--file", "-"},
			Stdin:  reader,
			Stdout: io.Discard,
			Stderr: io.Discard,
			Getenv: func(name string) string {
				return map[string]string{"BLOGCTL_BASE_URL": srv.URL, "BLOGCTL_CLIENT_ID": blogtest.ClientID, "BLOGCTL_CLIENT_SECRET": blogtest.ClientSecret}[name]
			},
			Version: "test",
		})
	}()
	select {
	case code := <-done:
		if code == cli.ExitOK {
			t.Fatal("interrupted command exited 0")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command kept waiting for stdin after the interrupt")
	}
}
