package autohttp

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/fortytw2/lounge"
	"github.com/jwfriese/autohttp/internal/httpsnoop"
)

type embeddedAssets struct {
	staticDir fs.FS
}

func newEmbeddedAssets(assets fs.FS, distDir string) (*embeddedAssets, error) {
	staticFS, err := fs.Sub(assets, distDir)
	if err != nil {
		return nil, err
	}

	return &embeddedAssets{
		staticDir: staticFS,
	}, nil
}

type Router struct {
	Routes     map[string]map[string]http.Handler
	starRoutes map[string]http.Handler

	embeddedAssets *embeddedAssets

	log lounge.Log

	enableHSTS         bool
	enableRouteMetrics bool

	defaultEncoder      Encoder
	defaultDecoder      Decoder
	defaultErrorHandler ErrorHandler
}

type RouterOption func(r *Router) error

func EnableHSTS(r *Router) error {
	r.enableHSTS = true
	return nil
}

func EnableRouteMetrics(r *Router) error {
	r.enableRouteMetrics = true
	return nil
}

func WithEmbeddedAssets(assets fs.FS, path string) func(r *Router) error {
	return func(r *Router) error {
		ea, err := newEmbeddedAssets(assets, path)
		if err != nil {
			return err
		}

		r.embeddedAssets = ea
		return nil
	}
}

func WithDefaultEncoder(e Encoder) func(r *Router) error {
	return func(r *Router) error {
		r.defaultEncoder = e
		return nil
	}
}

func WithDefaultDecoder(d Decoder) func(r *Router) error {
	return func(r *Router) error {
		r.defaultDecoder = d
		return nil
	}
}

func WithDefaultErrorHandler(eh ErrorHandler) func(r *Router) error {
	return func(r *Router) error {
		r.defaultErrorHandler = eh
		return nil
	}
}

var DefaultOptions = []RouterOption{
	WithDefaultDecoder(NewJSONDecoder()),
	WithDefaultEncoder(&JSONEncoder{}),
}

func NewRouter(log lounge.Log, routerOptions ...RouterOption) (*Router, error) {
	r := &Router{log: log, Routes: make(map[string]map[string]http.Handler), starRoutes: make(map[string]http.Handler)}
	for _, ro := range append(DefaultOptions, routerOptions...) {
		err := ro(r)
		if err != nil {
			return nil, err
		}
	}

	return r, nil

}

var validMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodDelete: true,
	http.MethodPatch:  true,
	http.MethodPost:   true,
	http.MethodPut:    true,
}

func (r *Router) Register(method string, path string, fn interface{}, middlewares []Middleware) error {
	if strings.Contains(path, "*") {
		if httpHandler, ok := fn.(http.Handler); ok {
			r.starRoutes[path] = httpHandler
			return nil
		}
	}

	if ok := validMethods[method]; !ok {
		return fmt.Errorf("invalid http method: %s", method)
	}

	_, ok := r.Routes[method]
	if !ok {
		r.Routes[method] = make(map[string]http.Handler)
	}

	_, ok = r.Routes[method][path]
	if ok {
		return errors.New("route already registered")
	}

	if httpHandler, ok := fn.(http.Handler); ok {
		r.Routes[method][path] = httpHandler
		return nil
	}

	h, err := NewHandler(r.log, r.defaultDecoder, r.defaultEncoder, middlewares, r.defaultErrorHandler, fn)
	if err != nil {
		return err
	}

	r.Routes[method][path] = h

	return nil
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.enableRouteMetrics {
		m := httpsnoop.CaptureMetrics(http.HandlerFunc(r.internalServeHTTP), w, req)
		r.log.Debugf("served %d bytes for %s %s in %s with code %d", m.Written, req.Method, req.URL.Path, m.Duration, m.Code)

		return
	}

	r.internalServeHTTP(w, req)
}

func (r *Router) internalServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodOptions {
		return
	}

	for path, handler := range r.starRoutes {
		pathPrefix := strings.ReplaceAll(path, "*", "")
		if strings.HasPrefix(req.URL.Path, pathPrefix) {
			handler.ServeHTTP(w, req)
			return
		}
	}

	method := strings.ToUpper(req.Method)
	routes, ok := r.Routes[method]
	if !ok {
		if method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		r.serveNotFound(w, req)
		r.cleanLeftovers(req)
		return
	}

	route, ok := routes[req.URL.Path]
	if !ok {
		r.serveNotFound(w, req)
		r.cleanLeftovers(req)
		return
	}

	route.ServeHTTP(w, req)
	r.cleanLeftovers(req)
}

// this is a bit of weirdness from production on Heroku
// some reverse proxies get really upset if you don't read
// the entire request body, and sometimes that happens to us here
func (r *Router) cleanLeftovers(req *http.Request) {
	if req.Body == nil || req.Body == http.NoBody {
		// do nothing
	} else {
		// chew up the rest of the body
		var buf bytes.Buffer
		buf.ReadFrom(req.Body)
		req.Body.Close()
	}
}
