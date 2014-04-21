package main

import (
	"fmt"
	"html/template"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	configFile  = "_config.toml"
	destDir = "_gen"
	templateDir = "_templates"
)

type Post struct {
	Title    string
	Contents template.HTML
}

type Category struct {
	*CategoryConfig
	Posts []*Post
}

type SitkinRoot struct {
	Categories map[string]*Category
}

type CategoryConfig struct {
	Name     string
	Template string
}

type TOMLConfig struct {
	Categories []*CategoryConfig
}

func LoadConfig(filename string) (*SitkinRoot, error) {
	config := &TOMLConfig{}
	_, err := toml.DecodeFile(filename, config)
	if err != nil {
		return nil, err
	}
	root := &SitkinRoot{
		Categories: make(map[string]*Category),
	}
	for _, c := range config.Categories {
		category := &Category{CategoryConfig: c}
		root.Categories[c.Name] = category
	}
	return root, nil
}

func main() {
	root, err := LoadConfig(configFile)
	if err != nil {
		fatal(err)
	}
	templates := template.New("root")
	templateFiles, err := filepath.Glob(filepath.Join(templateDir, "*.tmpl"))
	if err != nil {
		fatal(err)
	}
	// Can't just use templates.ParseGlob() because it will set the names to the full base name (i.e. with .tmpl
	// at the end).
	for _, f := range templateFiles {
		name := strings.TrimSuffix(filepath.Base(f), ".tmpl")
		contents, err := ioutil.ReadFile(f)
		if err != nil {
			fatal(err)
		}
		if _, err = templates.New(name).Parse(string(contents)); err != nil {
			fatal(err)
		}
	}
	_, err = templates.ParseGlob(filepath.Join("*.tmpl"))
	if err != nil {
		fatal(err)
	}

	post := &Post{
		Title:    "Hello World!",
		Contents: template.HTML("These are the contents"),
	}
	root.Categories["posts"].Posts = []*Post{post}

	topLevel, err := filepath.Glob("*.tmpl")
	if err != nil {
		fatal(err)
	}

	files, err := ioutil.ReadDir(".")
	if err != nil {
		fatal(err)
	}
	// * For each category, find a _catname directory and render it according to the category configuration
	// * For each file *.tmpl, render
	// * For each file and directory not starting with _, copy over directly
	for _, f := range files {
		if !strings.HasPrefix(f.Name(), "_") {
			if err := CopyFiles(filepath.Join(f.Name()), f.Name())
		}
	}
}

func fatal(args ...interface{}) {
	fmt.Fprintln(os.Stderr, args...)
	os.Exit(1)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}
