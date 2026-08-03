package mcp

import (
	"os"
	"path/filepath"
	"rts/index"
	"sync"
	"time"
)

type FileCache struct {
	mu	sync.Mutex
	entries	map[string]*cacheEntry
}

type cacheEntry struct {
	data	[]byte
	mtime	time.Time
}

func NewFileCache() *FileCache {
	return &FileCache{
		entries: make(map[string]*cacheEntry),
	}
}

func (c *FileCache) Read(path string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}

	if entry, ok := c.entries[path]; ok && entry.mtime.Equal(info.ModTime()) {
		return entry.data, true, nil
	}

	// Cache miss: read from disk and store
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}

	c.entries[path] = &cacheEntry{
		data: data,
		mtime: info.ModTime(),
	}

	return data, false, nil
}

type IndexCache struct {
	mu	sync.Mutex
	projectRoot	string
	loaded	*index.IndexFile
	loadedMtime	time.Time
}

func NewIndexCache(projectRoot string) *IndexCache{
	return &IndexCache{projectRoot: projectRoot}
}

// Get returns the parsed index
func (c *IndexCache) Get() (*index.IndexFile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	path := filepath.Join(c.projectRoot, ".rts", "symbols.json")
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	// Fresh: same mTime as what we loaded -> serve from memory.
	if c.loaded != nil && c.loadedMtime.Equal(info.ModTime()){
		return c.loaded, nil
	}

	// If index has been updated then re-fetch.
	idx, err := index.Load(c.projectRoot)
	if err != nil {
		return nil, err
	}

	c.loaded = idx
	c.loadedMtime = info.ModTime()
	return idx, nil
}