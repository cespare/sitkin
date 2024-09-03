package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/html"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

type sitkin struct {
	dir     string
	devMode bool
	verbose bool
	config  struct {
		Ignore   []string
		NoHash   []string
		FileSets []string
	}

	templates         map[string]*template.Template
	fileSets          []*fileSet
	templateFiles     []*templateFile
	textTemplateFiles []*textTemplateFile
	markdownFiles     []*markdownFile
	copyFiles         []*copyFile
	hashAssets        map[string]string // "/styles/x.css" -> "/styles/x.asdf123.css"

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

	s := &sitkin{
		dir:        dir,
		devMode:    devMode,
		verbose:    verbose,
		templates:  make(map[string]*template.Template),
		hashAssets: make(map[string]string),
		ctx: &context{
			DevMode:  devMode,
			FileSets: make(map[string]*fileSet),
		},
	}

	// Load config file, if it exists.
	f, err := os.Open(filepath.Join(sitkinDir, "config.json"))
	if err == nil {
		err := json.NewDecoder(f).Decode(&s.config)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("error loading config.json: %s", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	for _, glob := range s.config.Ignore {
		if _, err := path.Match(glob, ""); err != nil {
			return nil, fmt.Errorf("bad ignore glob %q: %s", glob, err)
		}
	}
	for _, glob := range s.config.NoHash {
		if _, err := path.Match(glob, ""); err != nil {
			return nil, fmt.Errorf("bad nohash glob %q: %s", glob, err)
		}
	}

	// Load templates.
	defaultTmpl, err := s.parseTemplateFile(filepath.Join(sitkinDir, "default.tmpl"))
	if err != nil {
		return nil, fmt.Errorf("error loading default template: %s", err)
	}
	tmplFiles, err := filepath.Glob(filepath.Join(sitkinDir, "*.tmpl"))
	if err != nil {
		return nil, fmt.Errorf("error listing templates: %s", err)
	}
	s.templates["default"] = defaultTmpl
	unusedTemplates := make(map[string]struct{})
	for _, name := range tmplFiles {
		tmplName := strings.TrimSuffix(filepath.Base(name), ".tmpl")
		if tmplName == "default" {
			continue
		}
		tmpl, err := s.parseTemplateFileWithDefault(name)
		if err != nil {
			return nil, fmt.Errorf("error loading template %s: %s", name, err)
		}
		s.templates[tmplName] = tmpl
		unusedTemplates[tmplName] = struct{}{}
	}

	// Load the file sets.
	for _, name := range s.config.FileSets {
		tmpl, ok := s.templates[name]
		if !ok {
			return nil, fmt.Errorf("no template for file set %s", name)
		}
		fsDir := filepath.Join(dir, name)
		fs, err := s.loadFileSet(fsDir, tmpl)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("no directory for file set %s", name)
			}
			return nil, err
		}
		s.fileSets = append(s.fileSets, fs)
		delete(unusedTemplates, name)
	}

	isFileSetName := func(name string) bool {
		for _, n := range s.config.FileSets {
			if n == name {
				return true
			}
		}
		return false
	}

	// Categorize all the rest of the files in the project.
	fis, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("error reading files in project dir: %s", err)
	}
	for _, fi := range fis {
		name := fi.Name() // basename, since fi came from readdir
		switch {
		case name == "sitkin" ||
			name == "gen" ||
			strings.HasPrefix(name, ".") ||
			isFileSetName(name):
			// Don't copy these.
		case strings.HasSuffix(name, ".tmpl"):
			tmpl, err := s.parseTemplateFileWithDefault(filepath.Join(dir, name))
			if err != nil {
				return nil, fmt.Errorf("error loading template %s: %s", name, err)
			}
			tf := &templateFile{
				name: strings.TrimSuffix(filepath.Base(name), ".tmpl"),
				tmpl: tmpl,
			}
			s.templateFiles = append(s.templateFiles, tf)
		case strings.HasSuffix(name, ".tpl"):
			tmpl, err := s.parseTextTemplateFile(filepath.Join(dir, name))
			if err != nil {
				return nil, fmt.Errorf("error loading text template %s: %s", name, err)
			}
			ttf := &textTemplateFile{
				name: strings.TrimSuffix(filepath.Base(name), ".tpl"),
				tmpl: tmpl,
			}
			s.textTemplateFiles = append(s.textTemplateFiles, ttf)
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
			copyFiles, hashAssets, err := s.loadCopyFiles(dir, name)
			if err != nil {
				return nil, fmt.Errorf("error loading files to copy from %s: %s", name, err)
			}
			s.copyFiles = append(s.copyFiles, copyFiles...)
			for _, pair := range hashAssets {
				s.hashAssets[pair[0]] = pair[1]
			}
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

	// Fill in context.
	for _, fs := range s.fileSets {
		s.ctx.FileSets[fs.name] = fs
	}

	if s.verbose {
		log.Println("Hashed assets:")
		var keys []string
		for k := range s.hashAssets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			log.Printf("  %s -> %s", k, s.hashAssets[k])
		}
	}

	return s, nil
}

func (s *sitkin) tmplFuncs() template.FuncMap {
	return template.FuncMap{
		"formatRFC3339": func(t time.Time) string { return t.Format(time.RFC3339) },
		"xmlEscape": func(b template.HTML) (string, error) {
			var buf strings.Builder
			if err := xml.EscapeText(&buf, []byte(b)); err != nil {
				return "", err
			}
			return buf.String(), nil
		},
		"link": s.link,
	}
}

func (s *sitkin) link(href string) string {
	if hashed, ok := s.hashAssets[href]; ok {
		return hashed
	}
	return href
}

func (s *sitkin) parseTemplateFile(name string) (*template.Template, error) {
	t, err := template.New("").Funcs(s.tmplFuncs()).ParseFiles(name)
	if err != nil {
		return nil, err
	}
	return t.Lookup(filepath.Base(name)).Option("missingkey=error"), nil
}

func (s *sitkin) parseTextTemplateFile(name string) (*texttemplate.Template, error) {
	funcs := texttemplate.FuncMap(s.tmplFuncs())
	t, err := texttemplate.New("").Funcs(funcs).ParseFiles(name)
	if err != nil {
		return nil, err
	}
	return t.Lookup(filepath.Base(name)).Option("missingkey=error"), nil
}

func (s *sitkin) parseTextTemplate(text string) (*texttemplate.Template, error) {
	funcs := texttemplate.FuncMap(s.tmplFuncs())
	t, err := texttemplate.New("").Funcs(funcs).Parse(text)
	if err != nil {
		return nil, err
	}
	return t.Option("missingkey=error"), nil
}

func (s *sitkin) parseTemplateFileWithDefault(name string) (*template.Template, error) {
	t, err := s.templates["default"].Clone()
	if err != nil {
		panic(err)
	}
	return t.ParseFiles(name)
}

type fileSet struct {
	name     string
	Files    []*markdownFile
	LastDate time.Time
}

type markdownFile struct {
	Name         string
	tmpl         *template.Template
	markdownTmpl *texttemplate.Template // templatized markdown
	Contents     template.HTML          // markdownTmpl -> markdown -> HTML

	// The remaining fields are not used for top-level markdown files.
	Date     time.Time
	Metadata map[string]interface{}
}

func (s *sitkin) loadFileSet(dir string, tmpl *template.Template) (*fileSet, error) {
	fis, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{})
	var files []*markdownFile
	for _, fi := range fis {
		name := fi.Name() // basename only, since this comes from readdir
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
		metadata, markdownTmpl, err := s.loadMarkdownMetadata(pth)
		if err != nil {
			return nil, fmt.Errorf("error loading markdown file %s: %s", pth, err)
		}
		md := &markdownFile{
			Name:         parts[1],
			tmpl:         tmpl,
			markdownTmpl: markdownTmpl,
			Date:         t,
			Metadata:     metadata,
		}
		if _, ok := names[parts[1]]; ok {
			return nil, fmt.Errorf("duplicate name (%s) in file set", parts[1])
		}
		names[parts[1]] = struct{}{}
		files = append(files, md)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Date.After(files[j].Date)
	})
	fs := &fileSet{
		name:  filepath.Base(dir),
		Files: files,
	}
	if len(files) > 0 {
		fs.LastDate = files[0].Date
	}
	return fs, nil
}

func (s *sitkin) loadMarkdownMetadata(pth string) (metadata map[string]interface{}, tmpl *texttemplate.Template, err error) {
	b, err := os.ReadFile(pth)
	if err != nil {
		return nil, nil, err
	}
	var (
		begin = []byte("<!--")
		end   = []byte("-->")
	)
	if bytes.HasPrefix(b, begin) {
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
	}
	tmpl, err = s.parseTextTemplate(string(b))
	if err != nil {
		return nil, nil, err
	}
	return metadata, tmpl, nil
}

type templateFile struct {
	name string
	tmpl *template.Template
}

type textTemplateFile struct {
	name string
	tmpl *texttemplate.Template
}

func (s *sitkin) loadMarkdownFile(name string, tmpl *template.Template) (*markdownFile, error) {
	markdownTmpl, err := s.parseTextTemplateFile(name)
	if err != nil {
		return nil, err
	}
	return &markdownFile{
		Name:         strings.TrimSuffix(filepath.Base(name), ".md"),
		tmpl:         tmpl,
		markdownTmpl: markdownTmpl,
	}, nil
}

type copyFile struct {
	srcPath string // relative to source dir
	dstPath string // relative to dst dir; same as srcPath unless this has a hash name
}

func (s *sitkin) loadCopyFiles(dir, name string) (copyFiles []*copyFile, hashAssets [][2]string, err error) {
	walk := func(pth string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relpath, err := filepath.Rel(dir, pth)
		if err != nil {
			panic(err) // shouldn't happen
		}
		for _, glob := range s.config.Ignore {
			match, err := path.Match(glob, filepath.ToSlash(relpath))
			if err != nil {
				panic(err)
			}
			if match {
				if fi.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if fi.IsDir() {
			return nil
		}
		cf := &copyFile{
			srcPath: relpath,
			dstPath: relpath,
		}
		hashName := true
		switch filepath.Ext(pth) {
		case ".html", "":
			hashName = false
		}
		for _, glob := range s.config.NoHash {
			match, err := path.Match(glob, filepath.ToSlash(relpath))
			if err != nil {
				panic(err) // already checked
			}
			if match {
				hashName = false
				break
			}
		}
		if hashName {
			var h string
			if s.devMode {
				h = "NOHASH"
			} else {
				h, err = fileHash(pth)
				if err != nil {
					return err
				}
			}
			ext := path.Ext(filepath.Base(relpath))
			cf.dstPath = strings.TrimSuffix(relpath, ext) + "." + h + ext
			hashAssets = append(hashAssets, [2]string{
				"/" + filepath.ToSlash(cf.srcPath),
				"/" + filepath.ToSlash(cf.dstPath),
			})
		}
		copyFiles = append(copyFiles, cf)
		return nil
	}
	if err := filepath.Walk(filepath.Join(dir, name), walk); err != nil {
		return nil, nil, err
	}
	return copyFiles, hashAssets, nil
}

func fileHash(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return base62Hash(h.Sum(nil)[:8]), nil
}

// base62Hash generates a 10-character base62 string from 8 bytes of a hash.
// This is fairly short, but enough for collision avoidance (about 60 bits).
func base62Hash(b []byte) string {
	var sb strings.Builder
	const hashLen = 10
	sb.Grow(hashLen)
	n := binary.BigEndian.Uint64(b)
	var m uint64
	for i := 0; i < hashLen; i++ {
		n, m = n/62, n%62
		sb.WriteByte(base62Alphabet[m])
	}
	return sb.String()
}

const base62Alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func (cf *copyFile) copy(srcDir, dstDir string) error {
	src := filepath.Join(srcDir, cf.srcPath)
	dst := filepath.Join(dstDir, cf.dstPath)
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}

	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := tempFile(parent, filepath.Base(dst), stat.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, f); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
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

func (s *sitkin) render() error {
	// Delete and recreate the gen dir.
	genDir := filepath.Join(s.dir, "gen")
	if err := os.RemoveAll(genDir); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("cannot remove existing gen dir: %s", err)
		}
	}
	if err := os.Mkdir(genDir, 0o755); err != nil {
		return fmt.Errorf("cannot create gen dir: %s", err)
	}

	// Render markdown. We do this separately, before rendering the
	// bottom-level templates, because they can access the data in the
	// rendered markdown. For example, a text template could iterate through
	// a fileset and access each file's Contents field.
	var buf bytes.Buffer
	for _, fs := range s.fileSets {
		for _, f := range fs.Files {
			buf.Reset()
			if err := f.markdownTmpl.Execute(&buf, nil); err != nil {
				return fmt.Errorf("error rendering markdown inside file set %q: %s", fs.name, err)
			}
			f.Contents = template.HTML(renderMarkdown(buf.Bytes()))
		}
	}
	for _, f := range s.markdownFiles {
		buf.Reset()
		if err := f.markdownTmpl.Execute(&buf, nil); err != nil {
			return fmt.Errorf("error rendering markdown file %s: %s", f.Name, err)
		}
		f.Contents = template.HTML(renderMarkdown(buf.Bytes()))
	}

	// Render file sets.
	for _, fs := range s.fileSets {
		if err := s.renderFileSet(fs); err != nil {
			return fmt.Errorf("error rendering file set %q: %s", fs.name, err)
		}
	}

	// Render top-level templates.
	for _, tf := range s.templateFiles {
		if err := s.renderTemplate(tf); err != nil {
			return fmt.Errorf("error rendering template %q: %s", tf.name, err)
		}
	}
	for _, ttf := range s.textTemplateFiles {
		if err := s.renderTextTemplate(ttf); err != nil {
			return fmt.Errorf("error rendering text template %q: %s", ttf.name, err)
		}
	}

	// Render top-level markdown files.
	for _, md := range s.markdownFiles {
		if err := s.renderMarkdown(md); err != nil {
			return fmt.Errorf("error rendering markdown file %q: %s", md.Name, err)
		}
	}

	// Copy assets.
	for _, cf := range s.copyFiles {
		if err := cf.copy(s.dir, genDir); err != nil {
			return err
		}
	}

	return nil
}

var markdownRenderer = goldmark.New(goldmark.WithExtensions(
	extension.GFM,
	extension.Typographer,
))

func renderMarkdown(input []byte) []byte {
	var buf bytes.Buffer
	if err := markdownRenderer.Convert(input, &buf); err != nil {
		// The errors should only come from writing to the output.
		panic(err)
	}
	return buf.Bytes()
}

func (s *sitkin) renderFileSet(fs *fileSet) error {
	dir := filepath.Join(s.dir, "gen", fs.name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	for _, md := range fs.Files {
		if err := s.renderFileSetMarkdown(dir, md); err != nil {
			return err
		}
	}
	return nil
}

// context is the common context to all templates.
type context struct {
	DevMode  bool
	FileSets map[string]*fileSet
}

func (s *sitkin) renderFileSetMarkdown(dir string, md *markdownFile) error {
	f, err := createFile(filepath.Join(dir, md.Name+".html"))
	if err != nil {
		return err
	}
	defer f.Close()
	ctx := struct {
		*context
		*markdownFile
	}{
		context:      s.ctx,
		markdownFile: md,
	}
	var buf bytes.Buffer
	if err := md.tmpl.Execute(&buf, ctx); err != nil {
		return err
	}
	if err := minifyHTML(f, &buf); err != nil {
		return err
	}
	return f.Close()
}

func (s *sitkin) renderTemplate(tf *templateFile) error {
	f, err := createFile(filepath.Join(s.dir, "gen", tf.name+".html"))
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	if err := tf.tmpl.Execute(&buf, s.ctx); err != nil {
		return err
	}
	if err := minifyHTML(f, &buf); err != nil {
		return err
	}
	return f.Close()
}

func (s *sitkin) renderTextTemplate(ttf *textTemplateFile) error {
	f, err := createFile(filepath.Join(s.dir, "gen", ttf.name))
	if err != nil {
		return err
	}
	defer f.Close()
	if err := ttf.tmpl.Execute(f, s.ctx); err != nil {
		return err
	}
	return f.Close()
}

func (s *sitkin) renderMarkdown(md *markdownFile) error {
	f, err := createFile(filepath.Join(s.dir, "gen", md.Name+".html"))
	if err != nil {
		return err
	}
	defer f.Close()
	ctx := struct {
		*context
		*markdownFile
	}{
		context:      s.ctx,
		markdownFile: md,
	}
	var buf bytes.Buffer
	if err := md.tmpl.Execute(&buf, ctx); err != nil {
		return err
	}
	if err := minifyHTML(f, &buf); err != nil {
		return err
	}
	return f.Close()
}

func createFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
}

var defaultMinify = minify.New()

func minifyHTML(w io.Writer, r io.Reader) error {
	return html.Minify(defaultMinify, w, r, nil)
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
		doBuild := func() { build(dir, true, *verbose) }
		if err := watchDir(dir, 500*time.Millisecond, doBuild, "gen"); err != nil {
			log.Fatalln("Error watching project dir for changes:", err)
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
