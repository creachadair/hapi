package hapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/creachadair/hapi"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestCheckMethod(t *testing.T) {
	var called bool
	h := httptest.NewServer(hapi.CheckMethod("POST",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}),
	))

	// A GET request should fail without triggering the handler.
	if rsp, err := h.Client().Get(h.URL); err != nil {
		t.Fatalf("Request failed: %v", err)
	} else if sc, want := rsp.StatusCode, http.StatusMethodNotAllowed; sc != want {
		t.Errorf("Response: got code %d, want %d", sc, want)
	}
	if called {
		t.Error("The handler was called, but should not have been")
		called = false
	}

	// A POST request should trigger the handler but not report an error.
	if rsp, err := h.Client().Post(h.URL, "text/plain", nil); err != nil {
		t.Fatalf("Request failed: %v", err)
	} else if sc, want := rsp.StatusCode, http.StatusOK; sc != want {
		t.Errorf("Response: got code %d, want %d", sc, want)
	}
	if !called {
		t.Error("The handler was not called, but should have been")
	}
}

func TestHandleJSON(t *testing.T) {
	type params struct {
		ID string `json:"id"`
	}
	type result struct {
		Count int `json:"count"`
	}

	var reqURL string
	h := httptest.NewServer(hapi.HandleJSON(func(ctx context.Context, p params) (result, error) {
		if got, want := p.ID, "test"; got != want {
			t.Errorf("Parameter ID: got %q, want %q", got, want)
		}
		reqURL = hapi.ContextPlumbing(ctx).Request().URL.String()
		return result{Count: 25}, nil
	}))

	call := hapi.CallJSON[params, result]("POST", h.URL+"/testpath")
	r, _, err := call(context.Background(), h.Client().Do, params{ID: "test"})
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if got, want := r.Count, 25; got != want {
		t.Errorf("Result count: got %d, want %d", got, want)
	}
	if got, want := reqURL, "/testpath"; got != want {
		t.Errorf("Request URL: got %q, want %q", got, want)
	}
}

func TestCallJSON(t *testing.T) {
	const jsonError = `{"json":true}`
	h := httptest.NewServer(hapi.HandleJSON(func(_ context.Context, z int) (bool, error) {
		switch z {
		case http.StatusOK:
			return true, nil
		case http.StatusTeapot:
			return false, hapi.JSONError{
				Code:  z,
				Value: json.RawMessage(jsonError),
			}
		}
		return false, hapi.Errorf(z, "error thing")
	}))

	call := hapi.CallJSON[int, bool]("POST", h.URL)

	// A successful call should report a true value.
	if r, _, err := call(context.Background(), h.Client().Do, 200); err != nil {
		t.Errorf("Call 200: unexpected error: %v", err)
	} else if !r {
		t.Error("Call 200: result should be true")
	}

	checkError := func(t *testing.T, arg int, ctype, want string) {
		t.Helper()
		r, rsp, err := call(context.Background(), h.Client().Do, arg)
		if err == nil {
			t.Fatalf("Call %v: got %v, want error", arg, r)
		}
		e, ok := err.(hapi.CallError)
		if !ok {
			t.Fatalf("Call %v: got err=%[1]T %[1]v, want CallError", arg, err)
		}

		// Note we ignore the Response field for comparison purposes.
		// Its contents are not generated by the code being tested.
		opt := cmpopts.IgnoreUnexported(hapi.CallError{})
		if diff := cmp.Diff(e, hapi.CallError{
			Code: arg,
			Body: []byte(want),
		}, opt); diff != "" {
			t.Errorf("Call %v error (-got, +want):\n%s", arg, diff)
		}
		if got := rsp.Header.Get("content-type"); got != ctype {
			t.Errorf("Call %v content type: got %q, want %q", arg, got, ctype)
		}
	}

	// An unsuccessful call should return a CallError.
	// This version has a plain text body.
	// N.B. trailing newline from http.Error
	checkError(t, 500, "text/plain; charset=utf-8", "error thing\n")

	// An unsuccessful call should return a CallError.
	// This version has a JSON body.
	checkError(t, 418, "application/json", jsonError)
}

func TestPlumbing(t *testing.T) {
	h := httptest.NewServer(hapi.HandleJSON(func(ctx context.Context, s string) (string, error) {
		p := hapi.ContextPlumbing(ctx)
		p.Header().Set("x-magic-pixies", "YES")
		p.SetResponseStatus(222)
		return s + " go", nil
	}))

	call := hapi.CallJSON[string, string]("POST", h.URL)

	r, rsp, err := call(context.Background(), h.Client().Do, "ok")
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if got, want := r, "ok go"; got != want {
		t.Errorf("Call result: got %q, want %q", got, want)
	}
	if got, want := rsp.StatusCode, 222; got != want {
		t.Errorf("Call status: got %d, want %d", got, want)
	}
	if got, want := rsp.Header.Get("x-magic-pixies"), "YES"; got != want {
		t.Errorf("Call response header: got %q, want %q", got, want)
	}
}

func TestEditRequestClient(t *testing.T) {
	h := httptest.NewServer(hapi.HandleJSON(func(ctx context.Context, s string) (string, error) {
		req := hapi.ContextPlumbing(ctx).Request()
		if auth := req.Header.Get("authorization"); auth != "open sesame" {
			return "", hapi.Errorf(http.StatusUnauthorized, "that is not the secret")
		}
		return "welcome " + s, nil
	}))

	call := hapi.CallJSON[string, string]("POST", h.URL)
	t.Run("EditOK", func(t *testing.T) {
		ec := hapi.EditRequest(h.Client().Do, func(r *http.Request) error {
			r.Header.Set("authorization", "open sesame")
			return nil
		})

		r, _, err := call(context.Background(), ec, "Ali Baba")
		if err != nil {
			t.Fatalf("Call failed; %v", err)
		}
		if got, want := r, "welcome Ali Baba"; got != want {
			t.Errorf("Call result: got %q, want %q", got, want)
		}
	})

	t.Run("EditError", func(t *testing.T) {
		testError := errors.New("computer says no")
		ec := hapi.EditRequest(h.Client().Do, func(r *http.Request) error {
			return testError
		})

		r, _, err := call(context.Background(), ec, "Keyser Soze")
		if !errors.Is(err, testError) {
			t.Errorf("Call: got (%v, %v), want %v", r, err, testError)
		}
	})
}

func TestJSONError(t *testing.T) {
	const testError = `{"error":"you dun fuggup"}`
	h := httptest.NewServer(hapi.HandleJSON(func(_ context.Context, _ int) (bool, error) {
		return false, hapi.JSONError{
			Code:  http.StatusNotFound,
			Value: json.RawMessage(testError),
		}
	}))

	call := hapi.CallJSON[int, bool]("POST", h.URL)
	r, _, err := call(context.Background(), h.Client().Do, 0)
	if ce, ok := err.(hapi.CallError); !ok {
		t.Errorf("Call: got (%+v, %+v), want CallError", r, err)
	} else if got := string(ce.Body); got != testError {
		t.Errorf("Call error: got %q, want %q", got, testError)
	} else if got, want := ce.Code, http.StatusNotFound; got != want {
		t.Errorf("Call: got status %d, want %d", got, want)
	}
}
