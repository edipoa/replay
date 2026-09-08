package obs

import (
	"embed"
	"net/http"
)

// Sponsor logos rendered in the strip under the live video on aovivo.*.
// To add one: drop an image in sponsors/ and append a row to sponsors below,
// then rebuild. Logos should be light / transparent-background (they sit on the
// navy bar). Keep them small — ~500 px wide is plenty for the bar.
//
//go:embed sponsors/*.png
var sponsorFS embed.FS

type sponsor struct {
	Name  string // alt text
	Img   string // served path, /sponsors/<file>
	URL   string // optional click-through; "" renders a non-link
	Plate bool   // true = white card behind the logo (for dark-on-light art)
}

var sponsors = []sponsor{
	{Name: "Faz o Simples", Img: "/sponsors/faz-o-simples.png"},
	{Name: "Tubo Oeste — Materiais de Construção", Img: "/sponsors/tubo-oeste.png", Plate: true},
}

// sponsorsHandler serves the embedded logo files at /sponsors/<file>. Cached a
// day — the filename is bumped when the art changes.
func (r *Recorder) sponsorsHandler() http.Handler {
	fs := http.FileServer(http.FS(sponsorFS))
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fs.ServeHTTP(w, req)
	})
}
