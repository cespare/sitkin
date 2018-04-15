package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestCopyDir(t *testing.T) {
	td := newTempDir(t)
	defer td.remove()

	var s sitkin

	td.writeFile("d1/a.html", "a")
	td.writeFile("d1/b.js", "b")
	td.writeFile("d1/x/c.css", "c")
	td.writeFile("d1/x/y/d.txt", "d")
	td.writeFile("d1/x/y/e", "e")
	td.writeFile("d1/x/y/z/f.html", "f")

	hashed, err := s.copyFiles(td.path("d2"), td.path("d1"))
	if err != nil {
		t.Fatal(err)
	}

	bjs := "d2/b-" + hashHex("b") + ".js"
	ccss := "d2/x/c-" + hashHex("c") + ".css"
	dtxt := "d2/x/y/d-" + hashHex("d") + ".txt"

	td.checkFile("d2/a.html", "a")
	td.checkFile(bjs, "b")
	td.checkFile(ccss, "c")
	td.checkFile(dtxt, "d")
	td.checkFile("d2/x/y/e", "e")
	td.checkFile("d2/x/y/z/f.html", "f")

	want := map[string]string{
		td.path("d1/b.js"):      td.path(bjs),
		td.path("d1/x/c.css"):   td.path(ccss),
		td.path("d1/x/y/d.txt"): td.path(dtxt),
	}
	if !reflect.DeepEqual(hashed, want) {
		t.Errorf("hashed: got %v; want %v", hashed, want)
	}
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

func TestSitkin(t *testing.T) {
	td := newTempDir(t)
	defer td.remove()

	td.writeFile(
		"sitkin/default.tmpl",
		`<html>
<head>
<link href="/assets/css/x.css" rel="stylesheet">
</head>
<body>
{{block "contents" .}}
{{.Contents}}
{{end}}
</body>
</html>
`,
	)
	td.writeFile(
		"sitkin/posts.tmpl",
		`{{define "contents"}}
{{.Metadata.title}}
{{.Contents}}
{{end}}
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
	td.writeFile("index.tmpl", `{{define "contents"}}
<ol>
{{range .FileSets.posts.Files}}
<li>{{.Metadata.title}}</li>
{{end}}
</ol>
{{end}}
`)
	td.writeFile("about.md", "# About\n\nabc")
	td.writeFile("foo.html", "<p>foo</p>")
	td.writeFile("assets/css/x.css", "css text")

	s, err := load(td.dir, false, false)
	if err != nil {
		t.Fatal("load failed:", err)
	}
	if err := s.render(); err != nil {
		t.Fatal("render failed:", err)
	}

	cssLink := "/assets/css/x-" + hashHex("css text") + ".css"
	td.checkFile(
		"gen/posts/hello-world.html",
		"<link href="+cssLink+" rel=stylesheet>Hello World<h1>Hello World</h1><p>123",
	)
	td.checkFile(
		"gen/index.html",
		"<link href="+cssLink+" rel=stylesheet><ol><li>Hello World</ol>",
	)
	td.checkFile(
		"gen/about.html",
		"<link href="+cssLink+" rel=stylesheet><h1>About</h1><p>abc",
	)
	td.checkFile("gen/foo.html", "<p>foo</p>")
	td.checkFile("gen/assets/css/x-"+hashHex("css text")+".css", "css text")
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
	if filepath.Dir(td.dir) == goTestTempDir() {
		// Let go test clean this up.
		return
	}
	if err := os.RemoveAll(td.dir); err != nil {
		td.t.Error(err)
	}
}

func (td tempDir) path(name string) string {
	return filepath.Join(td.dir, name)
}

func (td tempDir) writeFile(name, contents string) {
	td.t.Helper()
	pth := td.path(name)
	if err := os.MkdirAll(filepath.Dir(pth), 0755); err != nil {
		td.t.Fatal(err)
	}
	if err := ioutil.WriteFile(pth, []byte(contents), 0644); err != nil {
		td.t.Fatal(err)
	}
}

func (td tempDir) checkFile(name, contents string) {
	td.t.Helper()
	b, err := ioutil.ReadFile(td.path(name))
	if err != nil {
		td.t.Error(err)
		return
	}
	if got := string(b); got != contents {
		td.t.Errorf("for %s: got contents\n\n%s\n\nwant:\n\n%s\n", name, got, contents)
	}
}

func goTestTempDir() string {
	main := os.Args[0]
	if !strings.HasPrefix(main, os.TempDir()) {
		return ""
	}
	if !strings.HasSuffix(main, ".test") {
		return ""
	}
	dir := filepath.Dir(main)
	for d := dir; len(d) > 1; d = filepath.Dir(d) {
		if strings.HasPrefix(filepath.Base(d), "go-build") {
			return dir
		}
	}
	return ""
}
