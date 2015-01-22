package main

import (
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/cespare/cp"
)

const (
	configFile  = "_config.toml"
	destDir     = "_gen"
	templateDir = "_templates"
	layoutDir   = "_layouts"
)

type Entry struct {
	Title    string
	Slug     string
	Contents template.HTML
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
	serverFlag := flag.Bool("serve", false, "Start HTTP server")
	addr := flag.String("addr", "localhost:4747", "HTTP server addr")
	flag.Parse()
	if *serverFlag {
		log.Println("Now serving on", *addr)
		log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(handle)))
	}

	if len(os.Args) > 1 {
		if err := os.Chdir(os.Args[1]); err != nil {
			fatal(err)
		}
	}
	root, err := LoadConfig(configFile)
	if err != nil {
		fatal(err)
	}

	templateFiles, err := filepath.Glob(filepath.Join(templateDir, "*.tmpl"))
	if err != nil {
		fatal(err)
	}
	templates, err := template.ParseFiles(templateFiles...)
	if err != nil {
		fatal(err)
	}
	layoutFiles, err := filepath.Glob(filepath.Join(layoutDir, "*.tmpl"))
	if err != nil {
		fatal(err)
	}
	layouts := make(map[string]*template.Template)
	for _, f := range layoutFiles {
		t, err := templates.Clone()
		if err != nil {
			fatal(err)
		}
		layouts[tmplName(f)], err = t.ParseFiles(f)
		if err != nil {
			fatal(err)
		}
	}

	// TODO: TEMP ****************************
	entry := &Entry{
		Title:    "Hello World!",
		Slug:     "hello-world",
		Contents: template.HTML("These are the contents"),
	}
	root.Categories["posts"].Entries = []*Entry{entry}
	//****************************************

	files, err := ioutil.ReadDir(".")
	if err != nil {
		fatal(err)
	}
	if err := os.RemoveAll(destDir); err != nil {
		fatal(err)
	}
	if err := os.Mkdir(destDir, 0755); err != nil {
		fatal(err)
	}

	// For each category, find a _catname directory and render it according to the category configuration
	for _, category := range root.Categories {
		if err := category.Render(layouts, root); err != nil {
			fatal(err)
		}
	}

	// For each file *.tmpl, render
	// For each file and directory not starting with _, copy over directly
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "_") {
			continue
		}
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".tmpl") {
			if err := renderTopLevelTmpl(templates, root, f.Name()); err != nil {
				fatal(err)
			}
			continue
		}
		if err := cp.CopyAll(filepath.Join(destDir, f.Name()), f.Name()); err != nil {
			fatal(err)
		}
	}
}

func tmplName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".tmpl")
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

func fatal(args ...interface{}) {
	fmt.Fprintln(os.Stderr, args...)
	os.Exit(1)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}
