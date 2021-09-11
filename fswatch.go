package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

const debugWatch = false

func watchDir(dir string, delay time.Duration, fn func(), ignore string) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w := &watcher{
		w:      fw,
		dir:    dir,
		ignore: map[string]struct{}{filepath.Join(dir, ignore): {}},
		delay:  delay,
		fn:     fn,
	}
	if err := w.addDir(dir); err != nil {
		return err
	}
	errc := make(chan error)
	go func() { errc <- w.watch() }()
	select {
	case err := <-fw.Errors:
		return err
	case err := <-errc:
		return err
	}
}

type watcher struct {
	w      *fsnotify.Watcher
	dir    string
	ignore map[string]struct{}
	delay  time.Duration
	fn     func()
}

const chmodMask fsnotify.Op = ^fsnotify.Op(0) ^ fsnotify.Chmod

func (w *watcher) watch() error {
	timer := time.NewTimer(0)
	<-timer.C
	timerStarted := false
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-w.w.Events:
			if !ok {
				return nil
			}
			if debugWatch {
				log.Printf("Raw fsnotify event: %s", ev)
			}
			// Ignore events that are *only* CHMOD to work around Spotlight.
			if ev.Op&chmodMask == 0 {
				continue
			}
			name := filepath.Clean(ev.Name)
			if _, ok := w.ignore[name]; ok {
				if debugWatch {
					log.Println("Ignoring change to", name)
				}
				continue
			}
			if !timerStarted {
				timer.Reset(w.delay)
				timerStarted = true
			}
			if ev.Op&fsnotify.Create != 0 {
				stat, err := os.Stat(name)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					return err
				}
				if stat.IsDir() {
					if err := w.addDir(name); err != nil {
						return err
					}
				}
			}
		case <-timer.C:
			if debugWatch {
				log.Println("Calling watch func")
			}
			w.fn()
			timerStarted = false
		}
	}
}

func (w *watcher) addDir(dir string) error {
	return filepath.Walk(dir, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		if _, ok := w.ignore[name]; ok {
			if debugWatch {
				log.Println("Ignoring dir", name)
			}
			return filepath.SkipDir
		}
		if debugWatch {
			log.Println("Adding watch for", name)
		}
		if err := w.w.Add(name); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	})
}
