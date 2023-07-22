// Package hapi is a support library for writing HTTP API servers and clients
// using the standard net/http package.
package hapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Error wraps a standard error value with an HTTP status code that should be
// returned when the error is reported to a caller.
type Error struct {
	Code int   // HTTP status code
	Err  error // underlying error
}

func (e Error) Error() string   { return e.Err.Error() }
func (e Error) HTTPStatus() int { return e.Code }
func (e Error) Unwrap() error   { return e.Err }

// Errorf constructs an error value of concrete type Error that reports the
// specified HTTP status and diagnostic message (as fmt.Errorf).
func Errorf(code int, msg string, args ...any) error {
	return Error{Code: code, Err: fmt.Errorf(msg, args...)}
}

// JSONError is an error value that encodes to a JSON response body.
// The Code is used as the HTTP response status.
type JSONError struct {
	Code  int // HTTP status code
	Value any // error response body (encoded as JSON)
}

func (e JSONError) Error() string   { return fmt.Sprintf("[%d] %v", e.Code, e.Value) }
func (e JSONError) HTTPStatus() int { return e.Code }

// CallError is the concrete error type reported by a CallJSON caller when the
// response has a non-2xx HTTP status.
type CallError struct {
	Code int    // the HTTP status code from the response
	Body []byte // the contents of the response body

	text string
}

func (e CallError) Error() string   { return e.text }
func (e CallError) HTTPStatus() int { return e.Code }

// CheckMethod wraps an HTTP handler to check that the request method matches
// the given value. If not, it reports 405 Method not allowed; otherwise it
// delegates to h.
func CheckMethod(method string, h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, fmt.Sprintf("method %s not allowed", r.Method), http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	}
}

type httpPlumbingKey struct{}

// Plumbing carries HTTP request and response plumbing through a context.
// See the ContextPlumbing function.
type Plumbing struct {
	code int // default is 200 OK
	h    http.Header
	r    *http.Request
}

// SetResponseStatus sets the response status of the pending request.
func (p *Plumbing) SetResponseStatus(code int) { p.code = code }

// Header returns the response headers so the caller can edit them.
func (p *Plumbing) Header() http.Header { return p.h }

// Request returns the inbound request.
func (p *Plumbing) Request() *http.Request { return p.r }

// ContextPlumbing returns the HTTP plumbing associated with ctx, or nil if ctx
// is not associated with an HTTP request. The context passed to the callback
// by HandleJSON will always have this value.
func ContextPlumbing(ctx context.Context) *Plumbing {
	if v := ctx.Value(httpPlumbingKey{}); v != nil {
		return v.(*Plumbing)
	}
	return nil
}

// HandleJSON constructs an HTTP handler that calls fn. The parameters are
// decoded from the request body as JSON, and the result is encoded as JSON in
// the response body.
//
// If fn reports an error with concrete type JSONError, the error response body
// is sent as JSON; otherwise the error response is plain text.
//
// On success, the default HTTP status is 200 OK. The caller can override this
// using the SetResponseStatus method of the plumbing.  The body of fn can
// recover the HTTP plumbing from ctx using the hapi.ContextPlumbing function.
func HandleJSON[P, R any](fn func(context.Context, P) (R, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var params P
		if err := ReadJSON(r, &params); err != nil {
			http.Error(w, err.Error(), ErrorStatus(err))
			return
		}

		p := &Plumbing{code: http.StatusOK, h: w.Header(), r: r}
		ctx := context.WithValue(r.Context(), httpPlumbingKey{}, p)
		result, err := fn(ctx, params)
		if err != nil {
			var jerr JSONError
			if errors.As(err, &jerr) {
				WriteJSONStatus(w, jerr.Code, jerr.Value)
				return
			}
			http.Error(w, err.Error(), ErrorStatus(err))
			return
		}

		// The temporary here is necessary because we cannot do interface
		// satisfaction checks directly on type parameter R.
		// See https://gist.github.com/creachadair/e6b75324cf20745701cfc4bb8296171e.
		code := p.code
		var rc any = result
		if hs, ok := rc.(HTTPStatuser); ok {
			code = hs.HTTPStatus()
		}
		WriteJSONStatus(w, code, result)
	}
}

// CallJSON returns a function that calls an HTTP method with a JSON request
// body and expecting a JSON response body. If the HTTP request succeeds, it
// returns the HTTP response. The caller can use the presence of the response
// to distinguish errors.
//
// If the request succeeded with with a 2xx status, status, it returns the
// decoded body as the result and a nil error.
//
// If the HTTP request succeeded but reported a non-2xx status, it reports an
// error with concrete type CallError.
//
// As a special case, passing nil for the client uses http.DefaultClient.
func CallJSON[P, R any](method, url string) func(context.Context, HTTPClient, P) (R, *http.Response, error) {
	return func(ctx context.Context, cli HTTPClient, params P) (R, *http.Response, error) {
		var r0 R
		pdata, err := json.Marshal(params)
		if err != nil {
			return r0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(pdata))
		if err != nil {
			return r0, nil, err
		}
		req.Header.Set("content-type", "application/json")
		if cli == nil {
			cli = http.DefaultClient
		}
		rsp, err := cli.Do(req)
		if err != nil {
			return r0, nil, err
		}
		// Successful response: Decode the body as a result.
		if rsp.StatusCode >= 200 && rsp.StatusCode < 300 {
			var result R
			err := unmarshalJSON(rsp, &result)
			return result, rsp, err
		}

		// Some other response: Report a CallError with the raw body.
		return r0, rsp, newCallError(rsp)
	}
}

// ReadJSON unmarshals the body of r as JSON into v. It reports an error if the
// content-type declared by the request is not application/json, or if the body
// is not valid JSON.
func ReadJSON(r *http.Request, v any) error {
	if r.Header.Get("content-type") != "application/json" {
		return Errorf(http.StatusBadRequest, "invalid content type")
	}

	return json.NewDecoder(r.Body).Decode(v)
}

// WriteJSONStatus marshals v to JSON and writes it to w with the given status
// code, and content-type set to application/json.  An error encoding v is
// reported directly to w.
func WriteJSONStatus(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), ErrorStatus(err))
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	w.Write(data)
}

func unmarshalJSON(rsp *http.Response, v any) error {
	defer rsp.Body.Close()
	data, err := io.ReadAll(rsp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func newCallError(rsp *http.Response) CallError {
	defer rsp.Body.Close()
	data, _ := io.ReadAll(rsp.Body)
	return CallError{Code: rsp.StatusCode, Body: data, text: rsp.Status}
}

// HTTPStatuser is an optional interface that can be implemented by error types
// to provide an appropriate HTTP status code for the error.
// The Error and JSONError types in this package implement this interface.
type HTTPStatuser interface {
	HTTPStatus() int
}

// ErrorStatus returns an appropriate HTTP status code for err.
// If err == nil, the status is 200 OK.
// If err is an HTTPStatuser, its HTTPStatus method is used.
// Otherwise the status is 500 Internal server error.
func ErrorStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var hs HTTPStatuser
	if errors.As(err, &hs) {
		return hs.HTTPStatus()
	}
	return http.StatusInternalServerError
}

// HTTPClient is the subset of the http.Client interface used by this package.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// An EditRequestClient wraps an HTTP client with a callback that is called
// with each request. If the callback reports an error, the request fails;
// otherwise any changes the callback made to the request are forwarded to the
// underlying client. If the callback is nil, all requests are forwarded
// without modification.
type EditRequestClient struct {
	Client HTTPClient
	Edit   func(*http.Request) error
}

func (e EditRequestClient) Do(r *http.Request) (*http.Response, error) {
	if e.Edit != nil {
		if err := e.Edit(r); err != nil {
			return nil, err
		}
	}
	return e.Client.Do(r)
}
