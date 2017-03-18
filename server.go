package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// fs implements http.FileSystem
type fs string

func (f fs) Open(path string) (http.File, error) {
	if !strings.ContainsRune(path, '.') {
		path += ".html"
	}
	return os.Open(filepath.Join(string(f), path))
}

var handler = http.FileServer(fs(destDir))

func handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "not a GET request", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/" {
		http.ServeFile(w, r, filepath.Join(destDir, "index.html"))
		return
	}
	handler.ServeHTTP(w, r)
}
