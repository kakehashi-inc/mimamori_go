package workers

import (
	"encoding/json"
	"io/fs"
	"net/http"
)

// The frontend build has two entries (see vite.config.ts): the main
// application, which every browser downloads (also before login), and the
// administrator extension, which only administrators may download. The Vite
// build manifest, embedded with the frontend, tells which output files
// belong to which entry.
const (
	viteManifestPath = ".vite/manifest.json"
	viteMainEntry    = "index.html"
	viteAdminEntry   = "src/admin/entry.tsx"
)

// viteChunk is the part of a Vite manifest entry that is used here. File
// fields are paths relative to the build directory; Imports and
// DynamicImports are keys of other manifest entries.
type viteChunk struct {
	File           string   `json:"file"`
	CSS            []string `json:"css"`
	Assets         []string `json:"assets"`
	Imports        []string `json:"imports"`
	DynamicImports []string `json:"dynamicImports"`
}

// webAssets is what the server knows about the embedded frontend build.
type webAssets struct {
	// restricted holds the files (paths relative to the build directory)
	// that only the administrator entry uses. They are served only to an
	// administrator's session.
	restricted map[string]bool
	// adminModules are the URLs the main application loads for an
	// administrator (GET /ui/modules).
	adminModules []string
}

// loadWebAssets reads the build manifest from the embedded frontend. A
// build without a manifest (or without the administrator entry) restricts
// nothing and has no modules; this is logged once.
func (c *core) loadWebAssets() *webAssets {
	wa := &webAssets{restricted: map[string]bool{}, adminModules: []string{}}
	if c.spaFS == nil {
		return wa
	}
	data, err := fs.ReadFile(c.spaFS, viteManifestPath)
	if err != nil {
		c.logger.Printf("web: frontend build manifest not found (%v); no file is restricted to administrators and no administrator module is offered", err)
		return wa
	}
	var m map[string]viteChunk
	if err := json.Unmarshal(data, &m); err != nil {
		c.logger.Printf("web: frontend build manifest unreadable (%v); no file is restricted to administrators and no administrator module is offered", err)
		return wa
	}
	admin, ok := m[viteAdminEntry]
	if !ok || admin.File == "" {
		c.logger.Printf("web: frontend build manifest has no administrator entry; no file is restricted to administrators and no administrator module is offered")
		return wa
	}
	public := reachableFiles(m, viteMainEntry)
	public[viteMainEntry] = true
	for f := range reachableFiles(m, viteAdminEntry) {
		if !public[f] {
			wa.restricted[f] = true
		}
	}
	wa.adminModules = []string{"/" + admin.File}
	return wa
}

// reachableFiles returns every output file an entry of the manifest uses:
// its own file, CSS and assets, and those of every chunk it imports,
// statically or dynamically.
func reachableFiles(m map[string]viteChunk, entry string) map[string]bool {
	files := map[string]bool{}
	seen := map[string]bool{}
	var walk func(key string)
	walk = func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		ch, ok := m[key]
		if !ok {
			return
		}
		if ch.File != "" {
			files[ch.File] = true
		}
		for _, f := range ch.CSS {
			files[f] = true
		}
		for _, f := range ch.Assets {
			files[f] = true
		}
		for _, k := range ch.Imports {
			walk(k)
		}
		for _, k := range ch.DynamicImports {
			walk(k)
		}
	}
	walk(entry)
	return files
}

// handleUIModules answers GET /ui/modules: the URLs of the frontend
// modules the main application loads for the logged-in account. A viewer
// gets an empty list.
func (c *core) handleUIModules(w http.ResponseWriter, r *http.Request) {
	mods := []string{}
	if principalFrom(r).isAdmin() && c.assets != nil {
		mods = c.assets.adminModules
	}
	writeJSON(w, http.StatusOK, mods)
}
