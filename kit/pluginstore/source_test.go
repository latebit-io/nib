package pluginstore

import "testing"

func TestParseSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NIB_TEST_PLUGIN_DIR", dir)
	for in, want := range map[string]Source{
		dir:                         LocalSource(dir),
		"$NIB_TEST_PLUGIN_DIR":      LocalSource(dir),
		"./not-there":               LocalSource("./not-there"),
		"https://example.com/p.git": GitSource("https://example.com/p.git", ""),
		"git@example.com:o/p.git":   GitSource("git@example.com:o/p.git", ""),
		"owner/repo":                GitHubSource("owner/repo", ""),
	} {
		if got, err := ParseSource(in); err != nil || got != want {
			t.Errorf("ParseSource(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := ParseSource("just words"); err == nil {
		t.Error("a word that is no source must be refused")
	}
}
