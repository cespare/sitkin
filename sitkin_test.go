package main

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNiceDuration(t *testing.T) {
	for _, tt := range []struct {
		d    time.Duration
		want string
	}{
		{time.Nanosecond, "1ns"},
		{900 * time.Nanosecond, "900ns"},
		{1100 * time.Nanosecond, "1.1μs"},
		{5500 * time.Nanosecond, "5.5μs"},
		{10600 * time.Nanosecond, "10.6μs"},
		{105123 * time.Nanosecond, "105μs"},
		{900 * time.Microsecond, "900μs"},
		{1100 * time.Microsecond, "1.1ms"},
		{9900 * time.Microsecond, "9.9ms"},
		{10100 * time.Microsecond, "10.1ms"},
		{900 * time.Millisecond, "900ms"},
		{1000 * time.Millisecond, "1.0s"},
		{9900 * time.Millisecond, "9.9s"},
		{59123 * time.Millisecond, "59.1s"},
		{60123 * time.Millisecond, "1m0s"},
		{61123 * time.Millisecond, "1m1s"},
		{325 * time.Second, "5m25s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
		{time.Hour + time.Minute + time.Second + 100*time.Millisecond, "1h1m1.1s"},
	} {
		if got := niceDuration(tt.d); got != tt.want {
			t.Errorf("niceDuration(%s): got %s; want %s", tt.d, got, tt.want)
		}
	}
}

func TestSitkin(t *testing.T) {
	td := newTempDir(t)
	defer td.remove()

	td.writeFile(
		"sitkin/default.tmpl",
		`<html>
  <body>
    {{block "contents" . -}}
    {{.Contents}}
    {{- end}}
  </body>
</html>
`,
	)
	td.writeFile(
		"sitkin/posts.tmpl",
		`{{define "contents" -}}
{{.Metadata.title}}
{{.Contents}}
{{- end}}
`,
	)
	td.writeFile(
		"posts/2018-03-05.hello-world.md",
		`<!--
{
  "title": "Hello World"
}
-->
# Hello World

123
`,
	)
	td.writeFile("index.tmpl", `{{define "contents"}}index{{end}}`)
	td.writeFile("about.md", "# About\n\nabc")
	td.writeFile("foo.html", "<p>foo</p>")
	td.writeFile("assets/css/x.css", "not actually css")

	s, err := load(td.dir)
	if err != nil {
		t.Fatal("load failed:", err)
	}
	if err := s.render(); err != nil {
		t.Fatal("render failed:", err)
	}

	td.checkFile(
		"gen/posts/hello-world.html",
		`<html>
  <body>
    Hello World
<h1>Hello World</h1>

<p>123</p>

  </body>
</html>
`,
	)
	td.checkFile(
		"gen/index.html",
		`<html>
  <body>
    index
  </body>
</html>
`,
	)
	td.checkFile(
		"gen/about.html",
		`<html>
  <body>
    <h1>About</h1>

<p>abc</p>

  </body>
</html>
`,
	)
	td.checkFile("gen/foo.html", "<p>foo</p>")
	td.writeFile("gen/assets/css/x.css", "not actually css")
}

type tempDir struct {
	t   *testing.T
	dir string
}

func newTempDir(t *testing.T) tempDir {
	t.Helper()
	// Put the temp dir inside go test's temp directory (if we're running
	// under go test).
	dir, err := ioutil.TempDir(goTestTempDir(), "sitkin-test-")
	if err != nil {
		t.Fatal(err)
	}
	return tempDir{t: t, dir: dir}
}

func (td tempDir) remove() {
	td.t.Helper()
	if filepath.Base(td.dir) == goTestTempDir() {
		// Let go test clean this up.
		return
	}
	if err := os.RemoveAll(td.dir); err != nil {
		td.t.Error(err)
	}
}

func (td tempDir) writeFile(name, contents string) {
	td.t.Helper()
	pth := filepath.Join(td.dir, name)
	if err := os.MkdirAll(filepath.Dir(pth), 0755); err != nil {
		td.t.Fatal(err)
	}
	if err := ioutil.WriteFile(pth, []byte(contents), 0644); err != nil {
		td.t.Fatal(err)
	}
}

func (td tempDir) checkFile(name, contents string) {
	td.t.Helper()
	b, err := ioutil.ReadFile(filepath.Join(td.dir, name))
	if err != nil {
		td.t.Error(err)
		return
	}
	if got := string(b); got != contents {
		td.t.Errorf("for %s: got contents\n\n%s\n\nwant:\n\n%s\n", name, got, contents)
	}
}

func goTestTempDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	if filepath.Base(dir) == "_test" {
		return dir
	}
	return ""
}
