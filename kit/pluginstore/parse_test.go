package pluginstore

import "testing"

func TestSourceValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		src     Source
		wantErr bool
	}{
		{"local ok", LocalSource("/x"), false},
		{"local missing path", Source{Type: SourceLocal}, true},
		{"git ok", GitSource("https://h/r.git", ""), false},
		{"git missing url", Source{Type: SourceGit}, true},
		{"github ok", GitHubSource("owner/repo", "main"), false},
		{"github missing slash", GitHubSource("ownerrepo", ""), true},
		{"github empty owner", GitHubSource("/repo", ""), true},
		{"github empty repo", GitHubSource("owner/", ""), true},
		{"github extra segment", GitHubSource("owner/repo/extra", ""), true},
		{"npm ok", Source{Type: SourceNPM, Package: "@x/y"}, false},
		{"unknown type", Source{Type: "weird"}, true},
		{"empty type", Source{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.src.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate(%+v) err=%v, wantErr=%t", tc.src, err, tc.wantErr)
			}
		})
	}
}

func TestParseManifest(t *testing.T) {
	t.Parallel()
	m, err := ParseManifest([]byte(`{"name":"foo","version":"1.2.0","description":"d"}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "foo" || m.Version != "1.2.0" || m.Description != "d" {
		t.Errorf("got %+v", m)
	}
	if _, err := ParseManifest([]byte(`{"version":"1.0.0"}`)); err == nil {
		t.Errorf("expected error on missing name")
	}
	if _, err := ParseManifest([]byte(`not json`)); err == nil {
		t.Errorf("expected error on bad json")
	}
}

func TestParseMarketplace_SourceForms(t *testing.T) {
	t.Parallel()
	body := `{
      "name": "mp",
      "plugins": [
        { "name": "p-local", "source": "./plugins/p-local" },
        { "name": "p-gh", "source": { "source": "github", "repo": "o/r", "ref": "v1" } },
        { "name": "p-git", "source": { "source": "url", "url": "https://h/r.git", "ref": "main" } },
        { "name": "p-sub", "source": { "source": "git-subdir", "url": "https://h/m.git", "path": "tools/p", "ref": "v2" } }
      ]
    }`
	mkt, err := ParseMarketplace([]byte(body))
	if err != nil {
		t.Fatalf("ParseMarketplace: %v", err)
	}
	if mkt.Name != "mp" || len(mkt.Plugins) != 4 {
		t.Fatalf("unexpected marketplace %+v", mkt)
	}
	want := []Source{
		LocalSource("./plugins/p-local"),
		{Type: SourceGitHub, Repo: "o/r", Ref: "v1"},
		{Type: SourceGit, URL: "https://h/r.git", Ref: "main"},
		{Type: SourceGit, URL: "https://h/m.git", Subdir: "tools/p", Ref: "v2"},
	}
	for i, w := range want {
		if mkt.Plugins[i].Source != w {
			t.Errorf("plugin %d source = %+v, want %+v", i, mkt.Plugins[i].Source, w)
		}
	}

	if _, err := ParseMarketplace([]byte(`{"plugins":[]}`)); err == nil {
		t.Errorf("expected error on missing marketplace name")
	}
	// A structurally-broken entry is rejected at parse time, not at install.
	if _, err := ParseMarketplace([]byte(`{"name":"mp","plugins":[{"name":"","source":"./x"}]}`)); err == nil {
		t.Errorf("expected error on empty entry name")
	}
	if _, err := ParseMarketplace([]byte(`{"name":"mp","plugins":[{"name":"p","source":{"source":"github"}}]}`)); err == nil {
		t.Errorf("expected error on github source missing repo")
	}
}

func TestDeriveID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mkt, name, want string
	}{
		{"", "foo", "foo"},
		{"mp", "foo", "mp__foo"},
		{"", "foo/bar baz", "foo-bar-baz"},
		{"my mp", "foo", "my-mp__foo"},
	}
	for _, tc := range cases {
		if got := deriveID(tc.mkt, tc.name); got != tc.want {
			t.Errorf("deriveID(%q,%q) = %q, want %q", tc.mkt, tc.name, got, tc.want)
		}
	}
}
