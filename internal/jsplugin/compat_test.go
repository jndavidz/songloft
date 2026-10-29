package jsplugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"songloft/internal/version"
)

func withHostVersion(t *testing.T, v string) {
	t.Helper()
	previous := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = previous })
}

func TestCheckManifestCompatibility_MinHostVersion(t *testing.T) {
	withHostVersion(t, "1.2.3")

	cases := []struct {
		name string
		min  string
		want bool
	}{
		{"older requirement passes", "1.2.2", true},
		{"equal requirement passes", "1.2.3", true},
		{"v prefix passes", "v1.2.3", true},
		{"newer requirement fails", "1.3.0", false},
		{"major newer fails", "2.0.0", false},
		{"empty requirement passes", "", true},
		{"malformed requirement fails closed", "latest", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &PluginManifest{MinHostVersion: tc.min}
			err := CheckManifestCompatibility(m)
			if (err == nil) != tc.want {
				t.Fatalf("err = %v, want ok=%v", err, tc.want)
			}
		})
	}
}

func TestCheckManifestCompatibility_DevHost(t *testing.T) {
	withHostVersion(t, "dev")
	m := &PluginManifest{MinHostVersion: "99.0.0"}
	if err := CheckManifestCompatibility(m); err != nil {
		t.Fatalf("dev host should satisfy any minHostVersion: %v", err)
	}
}

func TestCheckManifestCompatibility_HostVersionNormalization(t *testing.T) {
	cases := []struct {
		host string
		min  string
		want bool
	}{
		{"v2.13.6", "2.13.6", true},
		{"2.13.6-3-gabc123", "2.13.6", true},
		{"v2.13.6-rc1", "2.13.7", false},
		{"garbage", "1.0.0", false},
		{"garbage", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.host+"/"+tc.min, func(t *testing.T) {
			withHostVersion(t, tc.host)
			err := CheckManifestCompatibility(&PluginManifest{MinHostVersion: tc.min})
			if (err == nil) != tc.want {
				t.Fatalf("err = %v, want ok=%v", err, tc.want)
			}
		})
	}
}

func TestCheckManifestCompatibility_Capabilities(t *testing.T) {
	withHostVersion(t, "1.2.3")

	if err := CheckManifestCompatibility(&PluginManifest{Capabilities: []string{"music-source", "page"}}); err != nil {
		t.Fatalf("known capabilities: %v", err)
	}
	if err := CheckManifestCompatibility(&PluginManifest{Capabilities: []string{"time-machine"}}); err == nil {
		t.Fatal("unknown capability must fail closed")
	}
	if err := CheckManifestCompatibility(&PluginManifest{Capabilities: []string{""}}); err == nil {
		t.Fatal("empty capability must fail closed")
	}
}

func TestInstallRejectsIncompatiblePlugin(t *testing.T) {
	withHostVersion(t, "1.2.3")
	pluginsDir, dataDir, repo, _ := setupTestEnv(t)
	pm := NewPackageManager(pluginsDir, dataDir, repo)

	manifest := testManifest("incompatible-plugin")
	manifest.MinHostVersion = "99.0.0"
	zipData := createTestPluginZip(t, manifest, simpleJSCode)

	if _, _, err := pm.InstallFromUpload(zipData); err == nil {
		t.Fatal("install must fail for incompatible minHostVersion")
	}
	if _, err := repo.GetByEntryPath(context.Background(), "incompatible-plugin"); err == nil {
		t.Fatal("incompatible plugin must not be recorded")
	}

	// capabilities 未知同样失败关闭。
	manifest = testManifest("unknown-capability-plugin")
	manifest.Capabilities = []string{"teleport"}
	zipData = createTestPluginZip(t, manifest, simpleJSCode)
	if _, _, err := pm.InstallFromUpload(zipData); err == nil {
		t.Fatal("install must fail for unsupported capability")
	}
}

func TestUpdateRejectsIncompatiblePlugin(t *testing.T) {
	withHostVersion(t, "1.2.3")
	pluginsDir, dataDir, repo, _ := setupTestEnv(t)
	pm := NewPackageManager(pluginsDir, dataDir, repo)

	manifest := testManifest("updatable-plugin")
	zipData := createTestPluginZip(t, manifest, simpleJSCode)
	plugin, _, err := pm.InstallFromUpload(zipData)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	incompatible := testManifest("updatable-plugin")
	incompatible.Version = "1.1.0"
	incompatible.MinHostVersion = "99.0.0"
	badZip := createTestPluginZip(t, incompatible, simpleJSCode+";//v1.1.0")
	if _, err := pm.Update(plugin.ID, badZip); err == nil {
		t.Fatal("update must fail for incompatible minHostVersion")
	}

	stored, err := repo.GetByID(context.Background(), plugin.ID)
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	if stored.Version != "1.0.0" {
		t.Fatalf("record must keep previous version, got %q", stored.Version)
	}
}

func TestAutoUpdateRejectsIncompatiblePlugin(t *testing.T) {
	withHostVersion(t, "1.2.3")
	pluginsDir, dataDir, repo, _ := setupTestEnv(t)
	pm := NewPackageManager(pluginsDir, dataDir, repo)

	var updatedZip []byte
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/update.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version":      "2.0.0",
			"download_url": server.URL + "/plugin.zip",
		})
	})
	mux.HandleFunc("/plugin.zip", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(updatedZip)
	})

	manifest := testManifest("auto-update-incompatible")
	manifest.UpdateURL = server.URL + "/update.json"
	zipData := createTestPluginZip(t, manifest, simpleJSCode)
	plugin, _, err := pm.InstallFromUpload(zipData)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	incompatible := testManifest("auto-update-incompatible")
	incompatible.Version = "2.0.0"
	incompatible.MinHostVersion = "99.0.0"
	incompatible.UpdateURL = manifest.UpdateURL
	updatedZip = createTestPluginZip(t, incompatible, simpleJSCode+";//v2.0.0")

	if _, err := pm.DownloadUpdate(plugin.ID, "", false); err == nil {
		t.Fatal("auto-update must fail closed for an incompatible release")
	}
	stored, err := repo.GetByID(context.Background(), plugin.ID)
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	if stored.Version != "1.0.0" {
		t.Fatalf("record must stay at 1.0.0, got %q", stored.Version)
	}
}

func TestLoadPluginRejectsIncompatibleRecord(t *testing.T) {
	withHostVersion(t, "1.2.3")
	pluginsDir, dataDir, repo, _ := setupTestEnv(t)

	manifest := testManifest("legacy-incompatible")
	zipData := createTestPluginZip(t, manifest, simpleJSCode)
	zipName := manifest.EntryPath + ".jsplugin.zip"
	if err := os.WriteFile(filepath.Join(pluginsDir, zipName), zipData, 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	manager := NewManager(repo, pluginsDir, dataDir, "", nil, nil)
	t.Cleanup(func() { manager.Close() })

	plugin := &JSPlugin{
		Name:           manifest.Name,
		Version:        manifest.Version,
		EntryPath:      manifest.EntryPath,
		Main:           manifest.Main,
		MinHostVersion: "99.0.0",
		Permissions:    manifest.Permissions,
		Status:         JSPluginStatusActive,
		FilePath:       zipName,
	}
	if err := repo.Create(context.Background(), plugin); err != nil {
		t.Fatalf("create record: %v", err)
	}

	err := manager.LoadPlugin(context.Background(), plugin)
	if err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("LoadPlugin error = %v, want incompatible", err)
	}
	if _, ok := manager.GetService(manifest.EntryPath); ok {
		t.Fatal("incompatible plugin must not be loaded")
	}
}

func TestLoadPluginFailsClosedWhenZipUnreadable(t *testing.T) {
	withHostVersion(t, "1.2.3")
	pluginsDir, dataDir, repo, _ := setupTestEnv(t)

	manager := NewManager(repo, pluginsDir, dataDir, "", nil, nil)
	t.Cleanup(func() { manager.Close() })

	plugin := &JSPlugin{
		Name:        "missing-zip",
		Version:     "1.0.0",
		EntryPath:   "missing-zip",
		Main:        "main.js",
		Permissions: []string{"songs.read"},
		Status:      JSPluginStatusActive,
		FilePath:    "missing-zip.jsplugin.zip",
	}
	if err := manager.LoadPlugin(context.Background(), plugin); err == nil {
		t.Fatal("missing plugin zip must fail the compatibility gate closed")
	}
	if _, ok := manager.GetService(plugin.EntryPath); ok {
		t.Fatal("plugin must not be loaded")
	}
}

func TestLoadPluginRejectsUnsupportedCapabilityInZip(t *testing.T) {
	withHostVersion(t, "1.2.3")
	pluginsDir, dataDir, repo, _ := setupTestEnv(t)

	manifest := testManifest("capability-incompatible")
	manifest.Capabilities = []string{"teleport"}
	zipData := createTestPluginZip(t, manifest, simpleJSCode)
	zipName := manifest.EntryPath + ".jsplugin.zip"
	if err := os.WriteFile(filepath.Join(pluginsDir, zipName), zipData, 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	manager := NewManager(repo, pluginsDir, dataDir, "", nil, nil)
	t.Cleanup(func() { manager.Close() })

	plugin := &JSPlugin{
		Name:        manifest.Name,
		Version:     manifest.Version,
		EntryPath:   manifest.EntryPath,
		Main:        manifest.Main,
		Permissions: manifest.Permissions,
		Status:      JSPluginStatusActive,
		FilePath:    zipName,
	}
	if err := repo.Create(context.Background(), plugin); err != nil {
		t.Fatalf("create record: %v", err)
	}

	if err := manager.LoadPlugin(context.Background(), plugin); err == nil {
		t.Fatal("LoadPlugin must reject unsupported capability declared in plugin.json")
	}
}
