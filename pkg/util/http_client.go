package util

import (
	"net/http"

	otelhttp "go.opentelemetry.io/contrib/instrumentation/net/http"
)

// Construct an HTTP client with instrumentation.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
}