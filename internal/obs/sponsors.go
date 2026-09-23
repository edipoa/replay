package obs

import (
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Sponsor logos are plain image files in Config.SponsorDir. Drop a file there
// and it shows up in the strip under the live video and in the footer of the
// exported clips; delete it and it goes away. Order = file name.

// isSponsorImage reports whether name has an extension ffmpeg + browsers handle.
func isSponsorImage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	}
	return false
}

// SponsorFiles returns the paths of the logo images in dir, sorted by name.
// A missing/unreadable dir yields nil, i.e. no sponsors.
func SponsorFiles(dir string) []string {
	entries, _ := os.ReadDir(dir) // ponytail: unreadable dir == no sponsors
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && isSponsorImage(e.Name()) {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths
}

type sponsor struct {
	Name string // alt text, from the file name
	Img  string // served path, /sponsors/<file>
}

// sponsorList is what the live page renders, re-read on every page load.
func (r *Recorder) sponsorList() []sponsor {
	var out []sponsor
	for _, p := range SponsorFiles(r.cfg.SponsorDir) {
		base := filepath.Base(p)
		name := strings.NewReplacer("-", " ", "_", " ").Replace(strings.TrimSuffix(base, filepath.Ext(base)))
		out = append(out, sponsor{Name: name, Img: "/sponsors/" + url.PathEscape(base)})
	}
	return out
}

// sponsorsHandler serves the logo files at /sponsors/<file> straight from
// SponsorDir (images only, no directory listing). no-cache = the browser
// revalidates on Last-Modified, so replacing a file under the same name works.
func (r *Recorder) sponsorsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := path.Base(req.URL.Path)
		if !isSponsorImage(name) {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.ServeFile(w, req, filepath.Join(r.cfg.SponsorDir, name))
	})
}
