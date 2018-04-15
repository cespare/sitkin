package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/cespare/fswatch"
	"github.com/tdewolff/minify"
	minifyhtml "github.com/tdewolff/minify/html"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"gopkg.in/russross/blackfriday.v2"
)

type sitkin struct {
	dir     string
	devMode bool
	verbose bool

	templates     map[string]*template.Template
	fileSets      []*fileSet
	templateFiles []*templateFile
	markdownFiles []*markdownFile
	forCopy       []string // files/dirs to copy directly (basename only)

	ctx *context
}

func load(dir string, devMode, verbose bool) (*sitkin, error) {
	// Initial sanity check.
	sitkinDir := filepath.Join(dir, "sitkin")
	stat, err := os.Stat(sitkinDir)
	if err != nil {
		if os.IsNotExist(err) {
			err = fmt.Errorf("%s does not appear to be a sitkin project (it does not contain a sitkin directory)", dir)
		}
		return nil, err
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("%s/sitkin is not a directory", dir)
	}

	// Load templates.
	defaultTmpl, err := template.ParseFiles(filepath.Join(sitkinDir, "default.tmpl"))
	if err != nil {
		return nil, fmt.Errorf("error loading default template: %s", err)
	}
	defaultTmpl = defaultTmpl.Option("missingkey=error")
	tmplFiles, err := filepath.Glob(filepath.Join(sitkinDir, "*.tmpl"))
	if err != nil {
		return nil, fmt.Errorf("error listing templates: %s", err)
	}
	s := &sitkin{
		dir:     dir,
		devMode: devMode,
		verbose: verbose,
		templates: map[string]*template.Template{
			"default": defaultTmpl,
		},
		ctx: &context{
			DevMode:  devMode,
			FileSets: make(map[string]*fileSetContext),
		},
	}
	unusedTemplates := make(map[string]struct{})
	for _, name := range tmplFiles {
		tmplName := strings.TrimSuffix(filepath.Base(name), ".tmpl")
		if tmplName == "default" {
			continue
		}
		tmpl, err := s.parseTemplateFile(name)
		if err != nil {
			return nil, fmt.Errorf("error loading template %s: %s", name, err)
		}
		s.templates[tmplName] = tmpl
		unusedTemplates[tmplName] = struct{}{}
	}

	// Categorize all the rest of the files in the project.
	fis, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("error reading files in project dir: %s", err)
	}
	for _, fi := range fis {
		name := fi.Name()
		switch {
		case name == "sitkin" || name == "gen" || strings.HasPrefix(name, "."):
			// Don't copy these.
		case fi.IsDir():
			if tmpl, ok := s.templates[name]; ok {
				fsDir := filepath.Join(dir, name)
				fs, err := s.loadFileSet(fsDir, tmpl)
				if err != nil {
					return nil, fmt.Errorf("error loading file set %s: %s", fsDir, err)
				}
				s.fileSets = append(s.fileSets, fs)
				delete(unusedTemplates, name)
			} else {
				s.forCopy = append(s.forCopy, name)
			}
		case strings.HasSuffix(name, ".tmpl"):
			tmpl, err := s.parseTemplateFile(filepath.Join(dir, name))
			if err != nil {
				return nil, fmt.Errorf("error loading template %s: %s", name, err)
			}
			tf := &templateFile{
				name: strings.TrimSuffix(filepath.Base(name), ".tmpl"),
				tmpl: tmpl,
			}
			s.templateFiles = append(s.templateFiles, tf)
		case strings.HasSuffix(name, ".md"):
			base := strings.TrimSuffix(name, ".md")
			tmpl, ok := s.templates[base]
			if ok {
				delete(unusedTemplates, base)
			} else {
				tmpl = defaultTmpl
			}
			md, err := s.loadMarkdownFile(filepath.Join(dir, name), tmpl)
			if err != nil {
				return nil, fmt.Errorf("error loading markdown file %s: %s", name, err)
			}
			s.markdownFiles = append(s.markdownFiles, md)
		default:
			s.forCopy = append(s.forCopy, name)
		}
	}

	var unused []string
	for name := range unusedTemplates {
		unused = append(unused, name)
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		log.Println("Warning: the following templates are not used:", unused)
	}

	// Fill in fileSetContexts.
	for _, fs := range s.fileSets {
		var fsctx fileSetContext
		for _, mf := range fs.files {
			fsctx.Files = append(fsctx.Files, &fileContext{
				Name:     mf.name,
				Date:     mf.date,
				Metadata: mf.metadata,
			})
		}
		s.ctx.FileSets[fs.name] = &fsctx
	}

	return s, nil
}

func (s *sitkin) parseTemplateFile(name string) (*template.Template, error) {
	t, err := s.templates["default"].Clone()
	if err != nil {
		panic(err)
	}
	t = t.Option("missingkey=error")
	return t.ParseFiles(name)
}

type fileSet struct {
	name  string
	files []*markdownFile
}

type markdownFile struct {
	name     string
	tmpl     *template.Template
	contents []byte

	// The remaining fields are not used for top-level markdown files.
	date     time.Time
	metadata map[string]interface{}
}

func (s *sitkin) loadFileSet(dir string, tmpl *template.Template) (*fileSet, error) {
	fis, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{})
	var files []*markdownFile
	for _, fi := range fis {
		name := fi.Name()
		pth := filepath.Join(dir, name)
		if fi.IsDir() {
			log.Println("Warning: ignoring unexpected dir", pth)
			continue
		}
		if !strings.HasSuffix(name, ".md") {
			log.Println("Warning: ignoring unexpected file", pth)
			continue
		}
		parts := strings.SplitN(strings.TrimSuffix(name, ".md"), ".", 2)
		if len(parts) != 2 {
			log.Printf("Warning: ignoring strangely-named file %s (name is missing date)", pth)
			continue
		}
		t, err := time.Parse("2006-01-02", parts[0])
		if err != nil {
			log.Printf("Warning: ignoring strangely-named file %s (invalid date %q)", pth, parts[0])
			continue
		}
		metadata, contents, err := loadMarkdownMetadata(pth)
		if err != nil {
			return nil, fmt.Errorf("error loading markdown file %s: %s", pth, err)
		}
		md := &markdownFile{
			name:     parts[1],
			tmpl:     tmpl,
			contents: contents,
			date:     t,
			metadata: metadata,
		}
		if _, ok := names[parts[1]]; ok {
			return nil, fmt.Errorf("duplicate name (%s) in file set", parts[1])
		}
		names[parts[1]] = struct{}{}
		files = append(files, md)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].date.After(files[j].date)
	})
	return &fileSet{
		name:  filepath.Base(dir),
		files: files,
	}, nil
}

func loadMarkdownMetadata(pth string) (metadata map[string]interface{}, contents []byte, err error) {
	b, err := ioutil.ReadFile(pth)
	if err != nil {
		return nil, nil, err
	}
	var (
		begin = []byte("<!--")
		end   = []byte("-->")
	)
	if !bytes.HasPrefix(b, begin) {
		return nil, b, nil
	}
	b = b[len(begin):]
	i := bytes.Index(b, end)
	if i < 0 {
		return nil, nil, errors.New("no closing --> to end metadata")
	}
	metadata = make(map[string]interface{})
	if err := json.Unmarshal(b[:i], &metadata); err != nil {
		return nil, nil, fmt.Errorf("error decoding metadata: %s", err)
	}
	b = b[i+len(end):]
	if len(b) > 0 && b[0] == '\n' {
		b = b[1:]
	}
	return metadata, b, nil
}

type templateFile struct {
	name string
	tmpl *template.Template
}

func (s *sitkin) loadMarkdownFile(name string, tmpl *template.Template) (*markdownFile, error) {
	b, err := ioutil.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return &markdownFile{
		name:     strings.TrimSuffix(filepath.Base(name), ".md"),
		tmpl:     tmpl,
		contents: b,
	}, nil
}

func (s *sitkin) render() error {
	// Delete and recreate the gen dir.
	genDir := filepath.Join(s.dir, "gen")
	if err := os.RemoveAll(genDir); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("cannot remove existing gen dir: %s", err)
		}
	}
	if err := os.Mkdir(genDir, 0755); err != nil {
		return fmt.Errorf("cannot create gen dir: %s", err)
	}

	// Copy assets.
	hashAssets := make(map[string]string) // "/styles/x.css" -> "/styles/x-asdf123.css"
	toURLPath := func(baseDir, pth string) string {
		rel, err := filepath.Rel(baseDir, pth)
		if err != nil {
			panic(err)
		}
		return "/" + filepath.ToSlash(rel)
	}
	for _, name := range s.forCopy {
		src := filepath.Join(s.dir, name)
		dst := filepath.Join(genDir, name)
		hashed, err := s.copyFiles(dst, src)
		if err != nil {
			return err
		}
		for from, to := range hashed {
			hashAssets[toURLPath(s.dir, from)] = toURLPath(genDir, to)
		}
	}
	if s.verbose {
		log.Println("Hashed assets:")
		var keys []string
		for k := range hashAssets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			log.Printf("  %s -> %s", k, hashAssets[k])
		}
	}

	// Render file sets.
	for _, fs := range s.fileSets {
		if err := s.renderFileSet(fs, hashAssets); err != nil {
			return fmt.Errorf("error rendering file set %q: %s", fs.name, err)
		}
	}

	// Render top-level templates.
	for _, tf := range s.templateFiles {
		if err := s.renderTemplate(tf, hashAssets); err != nil {
			return fmt.Errorf("error rendering template %q: %s", tf.name, err)
		}
	}

	// Render top-level markdown files.
	for _, md := range s.markdownFiles {
		if err := s.renderMarkdown(md, hashAssets); err != nil {
			return fmt.Errorf("error rendering markdown file %q: %s", md.name, err)
		}
	}

	return nil
}

func (s *sitkin) copyFiles(dst, src string) (map[string]string, error) {
	walk, hashed := s.copyWalk(dst, src)
	return hashed, filepath.Walk(src, walk)
}

func (s *sitkin) copyWalk(dst, src string) (filepath.WalkFunc, map[string]string) {
	hashed := make(map[string]string)
	walk := func(pth string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relpath, err := filepath.Rel(src, pth)
		if err != nil {
			panic(err) // shouldn't happen?
		}
		target := filepath.Join(dst, relpath)
		if info.IsDir() {
			return os.Mkdir(target, info.Mode())
		}
		hashName := !s.devMode
		if src == "favicon.ico" {
			hashName = false
		}
		switch filepath.Ext(pth) {
		case ".html", "":
			hashName = false
		}
		dstPath, err := copyFile(filepath.Dir(target), pth, hashName)
		if err != nil {
			return err
		}
		if hashName {
			hashed[pth] = dstPath
		}
		return nil
	}
	return walk, hashed
}

func copyFile(dstDir, src string, hashName bool) (string, error) {
	rf, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer rf.Close()
	rstat, err := rf.Stat()
	if err != nil {
		return "", err
	}
	if rstat.IsDir() {
		return "", errors.New("copyFile called on a directory")
	}

	base := filepath.Base(src)
	tmp, err := tempFile(dstDir, base, rstat.Mode())
	if err != nil {
		return "", err
	}
	var w io.Writer = tmp
	var h hash.Hash
	if hashName {
		h = sha256.New()
		w = io.MultiWriter(tmp, h)
	}
	if _, err := io.Copy(w, rf); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if hashName {
		ext := path.Ext(base)
		prefix := strings.TrimSuffix(base, ext)
		hashStr := hex.EncodeToString(h.Sum(nil)[:12])
		base = prefix + "-" + hashStr + ext
	}
	dst := filepath.Join(dstDir, base)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}

func tempFile(dir, prefix string, mode os.FileMode) (*os.File, error) {
	const numAttempts = 1000
	for i := 0; i < numAttempts; i++ {
		name := filepath.Join(dir, fmt.Sprintf("%s.tmp.%d", prefix, i))
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if os.IsExist(err) {
			continue
		}
		return f, err
	}
	return nil, fmt.Errorf("could not create temp file after %d attempts", numAttempts)
}

func (s *sitkin) renderFileSet(fs *fileSet, hashAssets map[string]string) error {
	dir := filepath.Join(s.dir, "gen", fs.name)
	if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	for _, md := range fs.files {
		if err := s.renderFileSetMarkdown(dir, md, hashAssets); err != nil {
			return err
		}
	}
	return nil
}

// context is the common context to all templates.
type context struct {
	DevMode  bool
	FileSets map[string]*fileSetContext
}

type fileSetContext struct {
	Files []*fileContext
}

type fileContext struct {
	Name     string
	Date     time.Time
	Metadata map[string]interface{}
}

func (s *sitkin) renderFileSetMarkdown(dir string, md *markdownFile, hashAssets map[string]string) error {
	f, err := os.Create(filepath.Join(dir, md.name+".html"))
	if err != nil {
		return err
	}
	defer f.Close()
	ctx := struct {
		*context
		Contents template.HTML
		Date     time.Time
		Metadata map[string]interface{}
	}{
		context:  s.ctx,
		Contents: template.HTML(blackfriday.Run(md.contents)),
		Date:     md.date,
		Metadata: md.metadata,
	}
	var buf bytes.Buffer
	if err := md.tmpl.Execute(&buf, ctx); err != nil {
		return err
	}
	if err := rewriteLinksAndMinify(f, &buf, hashAssets); err != nil {
		return fmt.Errorf("error rewriting hashed asset links: %s", err)
	}
	return f.Close()
}

func (s *sitkin) renderTemplate(tf *templateFile, hashAssets map[string]string) error {
	f, err := os.Create(filepath.Join(s.dir, "gen", tf.name+".html"))
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	if err := tf.tmpl.Execute(&buf, s.ctx); err != nil {
		return err
	}
	if err := rewriteLinksAndMinify(f, &buf, hashAssets); err != nil {
		return fmt.Errorf("error rewriting hashed asset links: %s", err)
	}
	return f.Close()
}

func (s *sitkin) renderMarkdown(md *markdownFile, hashAssets map[string]string) error {
	f, err := os.Create(filepath.Join(s.dir, "gen", md.name+".html"))
	if err != nil {
		return err
	}
	defer f.Close()
	ctx := struct {
		*context
		Contents template.HTML
	}{
		context:  s.ctx,
		Contents: template.HTML(blackfriday.Run(md.contents)),
	}
	var buf bytes.Buffer
	if err := md.tmpl.Execute(&buf, ctx); err != nil {
		return err
	}
	if err := rewriteLinksAndMinify(f, &buf, hashAssets); err != nil {
		return fmt.Errorf("error rewriting hashed asset links: %s", err)
	}
	return f.Close()
}

var defaultMinify = minify.New()

func rewriteLinksAndMinify(w io.Writer, r io.Reader, hashAssets map[string]string) error {
	doc, err := html.Parse(r)
	if err != nil {
		return err
	}
	rewriteAssetLinks(doc, hashAssets)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		err := html.Render(pw, doc)
		pw.CloseWithError(err)
		close(done)
	}()
	if err := minifyhtml.Minify(defaultMinify, w, pr, nil); err != nil {
		io.Copy(ioutil.Discard, pr)
		<-done
		return err
	}
	return nil
}

func rewriteAssetLinks(node *html.Node, hashAssets map[string]string) {
	rewriteTag(node, hashAssets)
	for n := node.FirstChild; n != nil; n = n.NextSibling {
		rewriteAssetLinks(n, hashAssets)
	}
}

func rewriteTag(node *html.Node, hashAssets map[string]string) {
	if node.Type != html.ElementNode {
		return
	}
	switch node.DataAtom {
	case atom.Img:
		rewriteAttr(node.Attr, "src", hashAssets)
	case atom.Link:
		rewriteAttr(node.Attr, "href", hashAssets)
	case atom.Script:
		rewriteAttr(node.Attr, "src", hashAssets)
	}
}

func rewriteAttr(attrs []html.Attribute, name string, hashAssets map[string]string) {
	for i := range attrs {
		attr := &attrs[i]
		if attr.Namespace != "" || attr.Key != name {
			continue
		}
		u, err := url.Parse(attr.Val)
		if err != nil {
			continue
		}
		if u.Host != "" {
			continue
		}
		if !strings.HasPrefix(u.Path, "/") {
			log.Printf("Warning: non-absolute local paths aren't handled (%s)", attr.Val)
			continue
		}
		if hashed, ok := hashAssets[u.Path]; ok {
			attr.Val = hashed
		}
	}
}

func main() {
	log.SetFlags(0)
	devAddr := flag.String("devaddr", "", `If given, operate in dev mode: serve at this HTTP address,
open it in a browser window, and rebuild files when they change`)
	verbose := flag.Bool("v", false, "Verbose mode")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:

    sitkin [flags] [dir]

where the flags are:

`)
		flag.PrintDefaults()
		fmt.Fprint(os.Stderr, `
If dir is not given, then the current directory is used.
`)
	}
	flag.Parse()

	var dir string
	switch flag.NArg() {
	case 0:
		dir = "."
	case 1:
		dir = flag.Arg(0)
	default:
		flag.Usage()
		os.Exit(1)
	}

	if *devAddr == "" {
		build(dir, false, *verbose)
		return
	}

	// Dev mode. Serve HTTP, open up a browser window, rebuild files on change.
	// Start by building once, synchronously.
	build(dir, true, *verbose)

	go func() {
		events, errs, err := fswatch.Watch(dir, 500*time.Millisecond, "gen")
		if err != nil {
			log.Fatalln("Cannot watch project dir for changes:", err)
		}
		go func() {
			err := <-errs
			log.Fatalln("Error watching project dir for changes:", err)
		}()
		for range events {
			build(dir, true, *verbose)
		}
	}()

	ln, err := net.Listen("tcp", *devAddr)
	if err != nil {
		log.Fatalln("Cannot listen on selected address:", err)
	}

	go func() {
		// Wait for the server to start before opening a browser window.
		u := "http://" + ln.Addr().String()
		var ok bool
		start := time.Now()
		const maxWait = time.Second
		for delay := time.Millisecond; time.Since(start) < maxWait; delay *= 2 {
			resp, err := http.Get(u)
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ok = true
				break
			}
		}
		if !ok {
			log.Printf("After waiting %s, no content served at %s", maxWait, u)
			return
		}
		if err := openBrowser(u); err != nil {
			log.Println("Error opening browser:", err)
		}
	}()

	fs := http.FileServer(http.Dir(filepath.Join(dir, "gen")))
	log.Fatal(http.Serve(ln, fs))

}

func build(dir string, devMode, verbose bool) {
	start := time.Now()
	s, err := load(dir, devMode, verbose)
	if err != nil {
		log.Println("Error loading sitkin project:", err)
		if !devMode {
			os.Exit(1)
		}
		return
	}
	if err := s.render(); err != nil {
		log.Println("Error rendering sitkin project:", err)
		if !devMode {
			os.Exit(1)
		}
		return
	}
	log.Println("Successfully built in", niceDuration(time.Since(start)))
}

func niceDuration(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < 100*time.Microsecond:
		return fmt.Sprintf("%.1fμs", float64(d.Nanoseconds())/1e3)
	case d < time.Millisecond:
		return fmt.Sprintf("%.0fμs", float64(d.Nanoseconds())/1e3)
	case d < 100*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d.Nanoseconds())/1e6)
	case d < time.Second:
		return fmt.Sprintf("%.0fms", float64(d.Nanoseconds())/1e6)
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		mins := int64(d.Minutes())
		d -= time.Duration(mins) * time.Minute
		return fmt.Sprintf("%dm%.0fs", mins, d.Seconds())
	default:
		// Don't really care about longer times.
		return d.String()
	}
}

func openBrowser(url string) error {
	var tool string
	switch runtime.GOOS {
	case "darwin":
		tool = "open"
	case "linux":
		tool = "xdg-open"
	default:
		return fmt.Errorf("don't know to to open a browser window on GOOS %q", runtime.GOOS)
	}

	cmd := exec.Command(tool, url)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	return cmd.Run()
}
