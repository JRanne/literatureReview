package sources

import "net/http"

// httpClient is the subset of *http.Client the sources need.
type httpClient = *http.Client
