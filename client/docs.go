package client

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// The page of the documentation is Swagger UI. Its files are not part of this program or of its source: the browser loads them from a
// location that is given (DocsUI), by default from a CDN, from the stream of version 5 (the newest 5.x.y). A moving URL cannot have
// integrity hashes; for an exact version that is known here (knownDocsUI) the page carries them, so that the browser refuses a file that
// is not the one of that version. Only the few lines that start the page are ours.

const (
	// defaultDocsUI is where the files of Swagger UI are, by default: the newest version 5 (npm package swagger-ui-dist).
	defaultDocsUI = "https://cdn.jsdelivr.net/npm/swagger-ui-dist@5"
)

// knownDocsUI are the locations of an exact version whose files are known: the integrity hashes of swagger-ui.css and swagger-ui-bundle.js
// (checked against the checksum that the npm registry publishes). Use such a location, for example with -docs-ui, to have the hashes.
var knownDocsUI = map[string][2]string{
	"https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.33.1": {
		"sha384-Ov4/wv3j2bmct8cDc5X4ngJZohVPzEmc6uDPH8WeljUxO5vtoykvMEfbu9Vh6RaW",
		"sha384-ZPehFMQommnnuaZ4rpxgkgTT2DKFVp4hZC/7pLit+9Lek9T1YGSo23eHFbvNkXkw",
	},
}

// DocsUIHasIntegrity reports whether the page checks the files of Swagger UI at the location against hashes.
func DocsUIHasIntegrity(base string) bool {
	_, ok := knownDocsUI[base]
	return ok
}

// docsInit starts Swagger UI on the description of the API. It is served by the program itself, because the policy of the page does not
// allow a script in the page.
const docsInit = `window.addEventListener("load", function () {
  window.ui = SwaggerUIBundle({
    url: "/openapi.json",
    dom_id: "#swagger-ui",
    presets: [SwaggerUIBundle.presets.apis],
    layout: "BaseLayout",
    deepLinking: true,
    persistAuthorization: false
  });
});
`

// ParseDocsUI checks the location of the files of Swagger UI (a URL of a directory that has swagger-ui.css and swagger-ui-bundle.js) and
// returns it without a closing slash. Empty is the default location.
func ParseDocsUI(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return defaultDocsUI, nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("docs-ui %q: use the URL of a directory with the files of Swagger UI, for example %s", s, defaultDocsUI)
	}
	return s, nil
}

// docsPage makes the page for a location of the files of Swagger UI.
func docsPage(base string) (page, csp string) {
	css, js := "", ""
	if h, ok := knownDocsUI[base]; ok {
		css = ` integrity="` + h[0] + `" crossorigin="anonymous"`
		js = ` integrity="` + h[1] + `" crossorigin="anonymous"`
	}
	page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>eas-client API</title>
<link rel="stylesheet" href="` + base + `/swagger-ui.css"` + css + `>
</head>
<body>
<div id="swagger-ui"></div>
<script src="` + base + `/swagger-ui-bundle.js"` + js + `></script>
<script src="/docs/docs-init.js"></script>
</body>
</html>
`
	u, _ := url.Parse(base)
	origin := u.Scheme + "://" + u.Host
	// the page may load files of its own origin and of the one of Swagger UI, talk to its own origin only, and not be shown in a frame
	csp = "default-src 'none'; script-src 'self' " + origin + "; style-src 'self' 'unsafe-inline' " + origin + "; img-src 'self' data: " + origin +
		"; font-src 'self' data: " + origin + "; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	return page, csp
}

// docsHandler serves /docs, the page, and /docs/docs-init.js, the script that starts it.
func docsHandler(base string) http.Handler {
	page, csp := docsPage(base)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, notAllowed("GET, HEAD"))
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		switch strings.Trim(strings.TrimPrefix(r.URL.Path, "/docs"), "/") {
		case "":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", csp)
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(page))
			}
		case "docs-init.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(docsInit))
			}
		default:
			writeError(w, newError(http.StatusNotFound, "not_found", "no such file"))
		}
	})
}
