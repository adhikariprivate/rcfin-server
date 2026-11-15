package jellyfin

import (
	"embed"
	"html/template"
	"net/http"
)

// TMDB's approved blue long logo, downloaded unmodified from its official
// Logos & Attribution page.
//
//go:embed assets/tmdb-logo.svg
var attributionAssets embed.FS

var creditsTemplate = template.Must(template.New("credits").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>About RCFin — Credits</title>
  <style>
    :root { color-scheme: dark; font: 16px/1.5 system-ui, sans-serif; }
    body { margin: 0; padding: 3rem 1.5rem; background: #10151b; color: #edf2f7; }
    main { max-width: 44rem; margin: auto; }
    h1 { margin: 0 0 2rem; font-size: 2rem; }
    section { padding: 1.5rem; border: 1px solid #39434e; border-radius: .5rem; }
    img { display: block; width: min(100%, 300px); height: auto; margin: 1.5rem 0; }
    a { color: #90cea1; }
  </style>
</head>
<body>
  <main>
    <h1>About RCFin</h1>
    <section aria-labelledby="tmdb-heading">
      <h2 id="tmdb-heading">Metadata and artwork</h2>
      <p>Movie and television metadata and images may be provided by:</p>
      <a href="https://www.themoviedb.org" aria-label="The Movie Database (TMDB)">
        <img src="/About/Credits/TMDB.svg" alt="The Movie Database (TMDB)">
      </a>
      <p>This product uses the TMDB API but is not endorsed or certified by TMDB.</p>
    </section>
  </main>
</body>
</html>`))

func (j *Jellyfin) creditsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if err := creditsTemplate.Execute(w, nil); err != nil {
		http.Error(w, "unable to render credits", http.StatusInternalServerError)
	}
}

func (j *Jellyfin) tmdbLogoHandler(w http.ResponseWriter, r *http.Request) {
	data, err := attributionAssets.ReadFile("assets/tmdb-logo.svg")
	if err != nil {
		http.Error(w, "TMDB logo is unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}
