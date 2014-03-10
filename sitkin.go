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
	meta, err := toml.DecodeFile(filename, config)
	if err != nil {
		return nil, err
	}
	if err := ValidateTOMLConfig(meta); err != nil {
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

func ValidateTOMLConfig(meta toml.MetaData) error {
	fmt.Printf("\033[01;34m>>>> meta: %#v\x1B[m\n", meta)
	return nil
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
	for _, t := range topLevel {
		fmt.Println(">>>", t)
		if err := templates.ExecuteTemplate(os.Stdout, t, root); err != nil {
			fatal(err)
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
