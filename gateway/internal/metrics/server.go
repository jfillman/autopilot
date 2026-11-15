package metrics

import (
	"net/http"
	"time"
)

// Handler answers GET /metrics (the query string is ignored, as Python's does) and nothing else: no other path, no other method.
// There is no authentication, as in Python: the port is not the MCP port, and a NetworkPolicy admits Prometheus to it alone.
func Handler(r *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/metrics" {
			http.NotFound(w, req)
			return
		}
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body := []byte(r.Render())
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	})
}

// Server is the metrics listener: timeouts on every connection (a slow client cannot hold a goroutine), small headers.
func Server(addr string, r *Registry) *http.Server {
	return &http.Server{Addr: addr, Handler: Handler(r), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
}
