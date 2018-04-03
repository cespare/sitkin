package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/cespare/cp"
	"github.com/cespare/fswatch"
	"gopkg.in/russross/blackfriday.v2"
)

type sitkin struct {
	dir           string
	templates     map[string]*template.Template
	fileSets      []*fileSet
	templateFiles []*templateFile
	markdownFiles []*markdownFile
	forCopy       []string // files/dirs to copy directly (basename only)

	ctx *context
}

func load(dir string, devMode bool) (*sitkin, error) {
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
		dir: dir,
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
		switch name {
		case "sitkin", "gen":
			continue
		}
		switch {
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

	// Render top-level markdown files.
	for _, md := range s.markdownFiles {
		if err := s.renderMarkdown(md); err != nil {
			return fmt.Errorf("error rendering markdown file %q: %s", md.name, err)
		}
	}

	// Copy remaining files.
	for _, name := range s.forCopy {
		src := filepath.Join(s.dir, name)
		dst := filepath.Join(genDir, name)
		if err := cp.CopyAll(dst, src); err != nil {
			return err
		}
	}
	return nil
}

func (s *sitkin) renderFileSet(fs *fileSet) error {
	dir := filepath.Join(s.dir, "gen", fs.name)
	if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	for _, md := range fs.files {
		if err := s.renderFileSetMarkdown(dir, md); err != nil {
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

func (s *sitkin) renderFileSetMarkdown(dir string, md *markdownFile) error {
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
	if err := md.tmpl.Execute(f, ctx); err != nil {
		return err
	}
	return f.Close()
}

func (s *sitkin) renderTemplate(tf *templateFile) error {
	f, err := os.Create(filepath.Join(s.dir, "gen", tf.name+".html"))
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tf.tmpl.Execute(f, s.ctx); err != nil {
		return err
	}
	return f.Close()
}

func (s *sitkin) renderMarkdown(md *markdownFile) error {
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
	if err := md.tmpl.Execute(f, ctx); err != nil {
		return err
	}
	return f.Close()
}

func main() {
	log.SetFlags(0)
	devAddr := flag.String("devaddr", "", `If given, operate in dev mode: serve at this HTTP address,
open it in a browser window, and rebuild files when they change`)
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
		build(dir, false)
		return
	}

	// Dev mode. Serve HTTP, open up a browser window, rebuild files on change.
	// Start by building once, synchronously.
	build(dir, true)

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
			build(dir, true)
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

func build(dir string, devMode bool) {
	start := time.Now()
	s, err := load(dir, devMode)
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
