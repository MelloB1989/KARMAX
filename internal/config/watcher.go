package config

import (
	"context"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

type Watcher struct {
	path    string
	log     *zap.Logger
	onReload func(*KarmaxConfig)
}

func NewWatcher(path string, log *zap.Logger, onReload func(*KarmaxConfig)) *Watcher {
	return &Watcher{
		path:     path,
		log:      log,
		onReload: onReload,
	}
}

func (w *Watcher) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	dir := filepath.Dir(w.path)
	if err := watcher.Add(dir); err != nil {
		watcher.Close()
		return err
	}

	go func() {
		defer watcher.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if filepath.Base(event.Name) != filepath.Base(w.path) {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
					continue
				}
				w.log.Info("config file changed, reloading")
				cfg, err := Load(w.path)
				if err != nil {
					w.log.Error("config reload failed, keeping old config", zap.Error(err))
					continue
				}
				w.onReload(cfg)
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				w.log.Error("config watcher error", zap.Error(err))
			}
		}
	}()

	return nil
}
