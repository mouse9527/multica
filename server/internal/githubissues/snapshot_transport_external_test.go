package githubissues_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/githubissues"
)

// TestSnapshotTransportResponseCarriesNoToken calls SnapshotTransport from
// outside the package, as ghsnapshot does, and searches everything reachable
// through the exported fields of the response for the token. net/http points
// Response.Request at the request it sent, so a response handed back as is
// would expose the Authorization header.
func TestSnapshotTransportResponseCarriesNoToken(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter){
		"ok": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"head":{"sha":"0123abcd"}}`))
		},
		"redirect": func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://api.github.com/repositories/42/pulls/7")
			w.WriteHeader(http.StatusMovedPermanently)
		},
		"unauthorized": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusUnauthorized)
		},
	}
	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			sent := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent++
				if r.Header.Get("Authorization") != "Bearer "+githubissues.TestToken {
					t.Errorf("GitHub did not get the token")
				}
				respond(w)
			}))
			defer srv.Close()
			transport := githubissues.NewSnapshotTransportForTest(t, srv.URL, "acme/widgets")

			req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/acme/widgets/pulls/7", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if sent != 1 {
				t.Fatalf("requests sent = %d, want 1", sent)
			}
			if path := findExported(reflect.ValueOf(resp), githubissues.TestToken[:12], "resp", map[uintptr]bool{}); path != "" {
				t.Fatalf("the token is reachable through %s", path)
			}
			if resp.Request != req {
				t.Fatal("the response must point at the caller's request")
			}
		})
	}
}

// findExported walks v through exported struct fields, pointers, interfaces,
// maps and slices, and returns the path to the first string or byte slice
// containing needle. Unexported fields are what callers cannot reach.
func findExported(v reflect.Value, needle, path string, seen map[uintptr]bool) string {
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), needle) {
			return path
		}
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return ""
		}
		seen[v.Pointer()] = true
		return findExported(v.Elem(), needle, path, seen)
	case reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return findExported(v.Elem(), needle, path, seen)
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() {
				if p := findExported(v.Field(i), needle, path+"."+f.Name, seen); p != "" {
					return p
				}
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			key := path + "[" + iter.Key().String() + "]"
			if p := findExported(iter.Key(), needle, key, seen); p != "" {
				return p
			}
			if p := findExported(iter.Value(), needle, key, seen); p != "" {
				return p
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			if strings.Contains(string(v.Bytes()), needle) {
				return path
			}
			return ""
		}
		for i := range v.Len() {
			if p := findExported(v.Index(i), needle, path+"["+strconv.Itoa(i)+"]", seen); p != "" {
				return p
			}
		}
	}
	return ""
}
