package httporigin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckEndpoint(t *testing.T) {
	for address, ok := range map[string]bool{
		"https://example.com/": true, "http://127.0.0.1:8/": true, "http://localhost/": true,
		"http://[::1]/": true, "http://[::1%25lo0]/": true,
		"http://example.com/": false, "http://10.0.0.1/": false, "file:///etc/passwd": false, "https://": false,
	} {
		if err := CheckEndpoint(address); (err == nil) != ok {
			t.Errorf("%s: %v; want accepted %v", address, err, ok)
		}
	}
}

func TestClient_KeepsHeadersAndRedirectsOnOrigin(t *testing.T) {
	var got []string
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = append(got, "other:"+r.Header.Get("Authorization"))
	}))
	defer other.Close()
	var own *httptest.Server
	own = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "own:"+r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, own.URL+"/done", http.StatusFound)
		case "/away":
			http.Redirect(w, r, other.URL+"/", http.StatusFound)
		}
	}))
	defer own.Close()
	c := Client(own.URL, map[string]string{"Authorization": "Bearer t"})
	for _, path := range []string{"/same", "/away"} {
		resp, err := c.Get(own.URL + path)
		if err == nil {
			if cerr := resp.Body.Close(); cerr != nil {
				t.Error(cerr)
			}
		}
		if (err == nil) != (path == "/same") {
			t.Errorf("%s: err = %v", path, err)
		}
	}
	resp, err := c.Get(other.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}
	if want := "own:Bearer t|own:Bearer t|own:Bearer t|other:"; strings.Join(got, "|") != want {
		t.Errorf("requests %q; want %q", got, want)
	}
}

func TestExpandHeaders(t *testing.T) {
	t.Setenv("NIB_TEST_HTTPORIGIN", "v")
	headers := map[string]string{"B": "x ${NIB_TEST_HTTPORIGIN}", "A": "$plain"}
	if got, err := ExpandHeaders(headers); err != nil || got["B"] != "x v" || got["A"] != "$plain" {
		t.Errorf("expand = %v, %v", got, err)
	}
	if key := ReadsEnv(headers); key != "B" {
		t.Errorf("ReadsEnv = %q; want B", key)
	}
	if _, err := ExpandHeaders(map[string]string{"A": "${NIB_TEST_HTTPORIGIN_UNSET}"}); err == nil {
		t.Error("an unset variable was accepted")
	}
}
