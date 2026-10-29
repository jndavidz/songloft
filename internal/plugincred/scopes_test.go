package plugincred

import "testing"

func TestRequiredScope(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		scope  string
		ok     bool
	}{
		{"songs list read", "GET", "/api/v1/songs", ScopeSongsRead, true},
		{"song detail read", "HEAD", "/api/v1/songs/12", ScopeSongsRead, true},
		{"song play read", "GET", "/api/v1/songs/12/play", ScopeSongsRead, true},
		{"song lyric read", "GET", "/api/v1/songs/12/lyric", ScopeSongsRead, true},
		{"song update write", "PUT", "/api/v1/songs/12", ScopeSongsWrite, true},
		{"song create write", "POST", "/api/v1/songs/remote", ScopeSongsWrite, true},
		{"song tags read", "GET", "/api/v1/songs/12/song-tags", ScopeTagsRead, true},
		{"song tags write", "PUT", "/api/v1/songs/12/song-tags", ScopeTagsWrite, true},
		{"tags list read", "GET", "/api/v1/song-tags", ScopeTagsRead, true},
		{"tags create write", "POST", "/api/v1/song-tags", ScopeTagsWrite, true},
		{"playlist read", "GET", "/api/v1/playlists/3/songs", ScopePlaylistsRead, true},
		{"playlist write", "DELETE", "/api/v1/playlists/3", ScopePlaylistsWrite, true},
		{"play history read", "GET", "/api/v1/play-history", ScopeSongsRead, true},
		{"play history delete denied", "DELETE", "/api/v1/play-history", "", false},
		{"proxy read", "GET", "/api/v1/proxy", ScopeNet, true},
		{"proxy transcode write", "POST", "/api/v1/proxy/transcode/hls/sessions", ScopeNet, true},
		{"settings denied", "GET", "/api/v1/settings/music-path", "", false},
		{"configs denied", "PUT", "/api/v1/configs/foo", "", false},
		{"scan denied", "POST", "/api/v1/scan", "", false},
		{"auth denied", "POST", "/api/v1/auth/logout", "", false},
		{"upgrade denied", "POST", "/api/v1/upgrade/start", "", false},
		{"plugin management denied", "GET", "/api/v1/jsplugins/", "", false},
		{"plugin health denied", "GET", "/api/v1/plugins/health", "", false},
		{"plugin self requires caller handling", "GET", "/api/v1/jsplugin/my-plugin/api", "", false},
		{"plugin self files requires caller handling", "GET", "/api/v1/jsplugin/my-plugin/files/a.mp3", "", false},
		{"unknown path denied", "GET", "/api/v1/whatever", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, ok := RequiredScope(tc.method, tc.path)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if scope != tc.scope {
				t.Fatalf("scope = %q, want %q", scope, tc.scope)
			}
		})
	}
}

func TestPluginSelfEntry(t *testing.T) {
	cases := []struct {
		path  string
		entry string
		ok    bool
	}{
		{"/api/v1/jsplugin/x", "x", true},
		{"/api/v1/jsplugin/x/", "x", true},
		{"/api/v1/jsplugin/x/api/foo", "x", true},
		{"/api/v1/jsplugin/", "", false},
		{"/api/v1/jsplugins/x", "", false},
		{"/api/v1/songs", "", false},
	}
	for _, tc := range cases {
		entry, ok := PluginSelfEntry(tc.path)
		if ok != tc.ok || entry != tc.entry {
			t.Fatalf("PluginSelfEntry(%q) = (%q, %v), want (%q, %v)", tc.path, entry, ok, tc.entry, tc.ok)
		}
	}
}

func TestHasScopeExact(t *testing.T) {
	scopes := []string{ScopeSongsRead}
	if !HasScope(scopes, ScopeSongsRead) {
		t.Fatal("expected songs.read")
	}
	// 不做前缀糖：songs.read 不覆盖 songs.write，net 不覆盖 net:insecure-tls。
	if HasScope(scopes, ScopeSongsWrite) {
		t.Fatal("songs.read must not cover songs.write")
	}
	if HasScope([]string{ScopeNet}, "net:insecure-tls") {
		t.Fatal("net must not cover net:insecure-tls")
	}
	if HasScope(nil, ScopeSongsRead) {
		t.Fatal("nil scopes must not match")
	}
}
