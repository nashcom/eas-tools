package exporter

import "github.com/nashcom/eas-tools/internal/listenaddr"

var endpointWhat = map[string]string{
	"/probe":         "the probe of a target: /probe?target=NAME, the metrics of one ActiveSync probe",
	"/metrics":       "the metrics of the exporter itself",
	"/health":        "liveness: answers OK",
	"/debug/state":   "the internal state as JSON: targets, counters, certificate, listeners",
	"/config/reload": "POST: check the configuration source now",
}

// Endpoints lists the paths of a listener with what a request needs, for the log of the start: the main listener (metricsPort false) or
// the metrics port. It is built from MainRoutes and MetricsRoutes, which the handlers are built from as well.
func (c *Config) Endpoints(metricsPort bool) []listenaddr.Endpoint {
	paths := MainRoutes
	if metricsPort {
		paths = MetricsRoutes
	}
	token, tokenDetail := c.tokenSummary()
	out := make([]listenaddr.Endpoint, 0, len(paths))
	for _, p := range paths {
		auth, detail := "none", "no token needed"
		switch p {
		case "/probe", "/debug/state", "/config/reload":
			auth, detail = token, tokenDetail
		}
		out = append(out, listenaddr.Endpoint{Path: p, What: endpointWhat[p], Auth: auth, Detail: detail})
	}
	return out
}

// tokenSummary is what a request that is protected by the token needs, for the log: a word (none, token, token-error) and the detail
// for debug level. It never shows the token.
func (c *Config) tokenSummary() (string, string) {
	header, err := tokenHeader(c.AuthTokenName)
	if err != nil {
		header = defaultTokenHeader
	}
	form := "header " + header + ": Bearer <token>"
	if header != defaultTokenHeader {
		form = "header " + header + ": <token>"
	}
	var from string
	switch {
	case c.AuthTokenFile != "":
		from = "file " + c.AuthTokenFile
	case c.AuthToken != "":
		from = "configuration"
	case c.AuthTokenEnv != "":
		from = "variable " + c.AuthTokenEnv
	}
	if from == "" {
		return "none", "no token is configured: every peer that allow_from lets in may use it"
	}
	if tok, err := c.authToken(); err != nil || tok == "" {
		return "token-error", "a token is configured (" + from + ") but it is empty or cannot be read"
	}
	return "token", form + "; from the " + from
}
