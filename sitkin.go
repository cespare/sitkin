package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/cespare/cp"
	"github.com/russross/blackfriday"
)

const (
	configFile  = "_config.toml"
	destDir     = "_gen"
	templateDir = "_templates"
	layoutDir   = "_layouts"
)

type Entry struct {
	Date     time.Time
	Title    string
	Slug     string
	N        int // put a number after the date to order posts from the same day
	Contents template.HTML
}

type frontMatter struct {
	Title string
}

type Category struct {
	CategoryConfig
	Entries []*Entry
}

type SitkinRoot struct {
	BaseTemplate string
	Categories   map[string]*Category
}

type CategoryConfig struct {
	Name   string
	Layout string
}

type TOMLConfig struct {
	BaseTemplate string
	Categories   []CategoryConfig
}

func LoadConfig(filename string) (*SitkinRoot, error) {
	config := &TOMLConfig{}
	_, err := toml.DecodeFile(filename, config)
	if err != nil {
		return nil, err
	}
	root := &SitkinRoot{
		BaseTemplate: config.BaseTemplate,
		Categories:   make(map[string]*Category),
	}
	for _, c := range config.Categories {
		category := &Category{CategoryConfig: c}
		root.Categories[c.Name] = category
	}
	return root, nil
}

func main() {
	start := time.Now()
	log.SetFlags(0)
	log.SetPrefix("sitkin: ")
	addr := flag.String("http", "", "If specified, run HTTP server on given addr")
	flag.Parse()

	if flag.NArg() > 0 {
		if err := os.Chdir(flag.Arg(0)); err != nil {
			log.Fatal(err)
		}
	}
	if *addr != "" {
		log.Println("Now serving on", *addr)
		log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(handle)))
	}
	root, err := LoadConfig(configFile)
	if err != nil {
		log.Fatal(err)
	}

	templates, err := template.ParseGlob(filepath.Join(templateDir, "*.tmpl"))
	if err != nil {
		log.Fatal(err)
	}
	layoutFiles, err := filepath.Glob(filepath.Join(layoutDir, "*.tmpl"))
	if err != nil {
		log.Fatal(err)
	}
	layouts := make(map[string]*template.Template)
	for _, f := range layoutFiles {
		t, err := templates.Clone()
		if err != nil {
			log.Fatal(err)
		}
		layouts[tmplName(f)], err = t.ParseFiles(f)
		if err != nil {
			log.Fatal(err)
		}
	}

	for _, category := range root.Categories {
		if err := category.LoadEntries(); err != nil {
			log.Fatal(err)
		}
	}

	files, err := ioutil.ReadDir(".")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.RemoveAll(destDir); err != nil {
		log.Fatal(err)
	}
	if err := os.Mkdir(destDir, 0755); err != nil {
		log.Fatal(err)
	}

	// For each category, find a _catname directory and render it according
	// to the category configuration.
	for _, category := range root.Categories {
		if err := category.Render(layouts, root); err != nil {
			log.Fatal(err)
		}
	}

	// Render each .tmpl file.
	// For each other file and directory not starting with _,
	// copy over directly.
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "_") || strings.HasPrefix(f.Name(), ".") {
			continue
		}
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".tmpl") {
			if err := renderTopLevelTmpl(templates, root, f.Name()); err != nil {
				log.Fatal(err)
			}
			continue
		}
		if err := cp.CopyAll(filepath.Join(destDir, f.Name()), f.Name()); err != nil {
			log.Fatal(err)
		}
	}
	log.Println("generated site in", time.Since(start))
}

func tmplName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".tmpl")
}

func (c *Category) LoadEntries() error {
	paths, err := filepath.Glob(filepath.Join("_"+c.Name, "*.md"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		entry, err := c.LoadEntry(path)
		if err != nil {
			return err
		}
		c.Entries = append(c.Entries, entry)
	}
	// Sort entries by date (newest to oldest), breaking ties by
	// entry number (largest to smallest) and then by title.
	sort.Slice(c.Entries, func(i, j int) bool {
		e0, e1 := c.Entries[i], c.Entries[j]
		if e0.Date.After(e1.Date) {
			return true
		}
		if e0.Date.Before(e1.Date) {
			return false
		}
		if e0.N > e1.N {
			return true
		}
		if e0.N < e1.N {
			return false
		}
		return e0.Title > e1.Title
	})
	return nil
}

var entryNameRegexp = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(?:-(\d+))?-(.*)\.md$`)

func (c *Category) LoadEntry(path string) (*Entry, error) {
	base := filepath.Base(path)
	matches := entryNameRegexp.FindStringSubmatch(base)
	if len(matches) == 0 {
		return nil, fmt.Errorf("entry name %q is the incorrect format", path)
	}
	date, err := time.Parse("2006-01-02", matches[1])
	if err != nil {
		return nil, fmt.Errorf("bad date %q", matches[1])
	}
	entry := &Entry{
		Date: date,
		Slug: matches[3],
	}
	if matches[2] != "" {
		entry.N, err = strconv.Atoi(matches[2])
		if err != nil {
			return nil, fmt.Errorf("bad post numbers %q", matches[2])
		}
	}
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	front, text, err := splitFrontMatter(b)
	if err != nil {
		return nil, fmt.Errorf("malformed front matter: %s", err)
	}
	var fm frontMatter
	if _, err := toml.Decode(string(front), &fm); err != nil {
		return nil, fmt.Errorf("bad front matter: %s", err)
	}
	entry.Title = fm.Title
	entry.Contents = template.HTML(renderMarkdown(text))
	return entry, nil
}

func splitFrontMatter(b []byte) (front, text []byte, err error) {
	const (
		markBegin = "<!--\n"
		markEnd   = "-->\n"
	)
	if !bytes.HasPrefix(b, []byte(markBegin)) {
		return nil, b, nil
	}
	b = b[len(markBegin):]
	i := bytes.Index(b, []byte(markEnd))
	if i < 0 {
		return nil, nil, errors.New("no closing --> to end front matter")
	}
	return b[:i], b[i+len(markEnd):], nil
}

const (
	markdownExtensions = blackfriday.EXTENSION_TABLES |
		blackfriday.EXTENSION_FENCED_CODE |
		blackfriday.EXTENSION_AUTOLINK |
		blackfriday.EXTENSION_STRIKETHROUGH

	markdownHTMLOptions = blackfriday.HTML_USE_SMARTYPANTS |
		blackfriday.HTML_SMARTYPANTS_DASHES
)

var markdownRenderer = blackfriday.HtmlRenderer(markdownHTMLOptions, "", "")

func renderMarkdown(text []byte) []byte {
	return blackfriday.Markdown(text, markdownRenderer, markdownExtensions)
}

func (c *Category) Render(layouts map[string]*template.Template, root *SitkinRoot) error {
	layout, ok := layouts[c.Layout]
	if !ok {
		return fmt.Errorf("no such layout found: %q", c.Layout)
	}
	for _, entry := range c.Entries {
		if err := os.Mkdir(filepath.Join(destDir, c.Name), 0755); err != nil {
			return err
		}
		if err := c.RenderEntry(entry, layout, root); err != nil {
			return err
		}
	}
	return nil
}

func (c *Category) RenderEntry(entry *Entry, layout *template.Template, root *SitkinRoot) error {
	f, err := os.Create(filepath.Join(destDir, c.Name, entry.Slug+".html"))
	if err != nil {
		return err
	}
	defer func() {
		e := f.Close()
		if err == nil {
			err = e
		}
	}()
	return layout.ExecuteTemplate(f, root.BaseTemplate, entry)
}

func renderTopLevelTmpl(templates *template.Template, root *SitkinRoot, path string) (err error) {
	tmpl, err := templates.Clone()
	if err != nil {
		return err
	}
	tmpl, err = tmpl.ParseFiles(path)
	if err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(destDir, tmplName(path)+".html"))
	if err != nil {
		return err
	}
	defer func() {
		e := f.Close()
		if err == nil {
			err = e
		}
	}()
	return tmpl.ExecuteTemplate(f, root.BaseTemplate, root)
}
