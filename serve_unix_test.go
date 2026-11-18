//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
)

// TestRefreshPaths checks which paths the daemon touches through its mount for
// a change it made behind the NFS client, on a folder standing in for the
// mount: each changed file and every directory down to it, once, with the
// mode the daemon reports rather than the one the mount shows, and never a
// symlink's target, a path that is gone, or anything outside the checkout.
func TestRefreshPaths(t *testing.T) {
	mount := t.TempDir()
	for _, dir := range []string{"co/a/b", "co/tree/deep", "other"} {
		if err := os.MkdirAll(filepath.Join(mount, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"co/a/b/file", "co/a/target", "co/tree/deep/file", "other/file"} {
		if err := os.WriteFile(filepath.Join(mount, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("target", filepath.Join(mount, "co/a/link")); err != nil {
		t.Fatal(err)
	}
	// The daemon's view: regular files are 0644 there, whatever the mount shows.
	var asked []string
	lstat := func(name string) (os.FileInfo, error) {
		asked = append(asked, name)
		info, err := os.Lstat(filepath.Join(mount, filepath.FromSlash(name)))
		if err != nil || !info.Mode().IsRegular() {
			return info, err
		}
		return modeInfo{info, 0o644}, nil
	}
	refreshPaths(mount, "co", []string{"a/b/file", "a/link", "a/gone", "a/b/file"}, []string{"tree"}, lstat)

	if want := []string{"co", "co/tree", "co/a", "co/a/b", "co/a/b/file", "co/a/link", "co/a/gone"}; !slices.Equal(asked, want) {
		t.Errorf("refreshed %v, want each path once, directories first: %v", asked, want)
	}
	for name, want := range map[string]os.FileMode{
		"co/a/b/file":       0o644, // the daemon's mode, not the 0600 the mount showed
		"co/a/target":       0o600, // only reachable through the symlink
		"co/tree/deep/file": 0o600, // below a tree's root
		"other/file":        0o600,
	} {
		info, err := os.Stat(filepath.Join(mount, name))
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s has mode %v, %v; want %v", name, info.Mode().Perm(), err, want)
		}
	}
}

// modeInfo reports a file with another permission mode.
type modeInfo struct {
	os.FileInfo
	perm os.FileMode
}

func (m modeInfo) Mode() os.FileMode { return m.FileInfo.Mode()&^os.ModePerm | m.perm }

// TestServeListensWhileLoading holds serve's state load on a named pipe: its
// admin socket accepts during the load and holds the client's hello, and a
// load that fails closes the held connection, so the client fails at once.
func TestServeListensWhileLoading(t *testing.T) {
	// A short path: a Unix socket path is limited to about 100 bytes.
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	stateFile := filepath.Join(state, "state.json")
	if err := syscall.Mkfifo(stateFile, 0o600); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- (&serveCmd{}).Run(&cli{state: state, mount: filepath.Join(state, "mnt")})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !accepting(state) {
		select {
		case err := <-served:
			t.Fatalf("serve returned before it listened: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no admin socket while the state loads")
		}
		time.Sleep(10 * time.Millisecond)
	}
	dialed := make(chan error, 1)
	go func() {
		client, err := admin.Dial(socketPath(state), adminProtocol)
		if err == nil {
			client.Close()
		}
		dialed <- err
	}()
	select {
	case err := <-dialed:
		t.Fatalf("hello answered while the state loads: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// Opening the pipe for writing releases serve's read of it.
	if err := os.WriteFile(stateFile, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err == nil || !strings.Contains(err.Error(), "state.json") {
			t.Fatalf("serve with an unreadable state: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve still loading after the state was written")
	}
	select {
	case err := <-dialed:
		if err == nil {
			t.Fatal("hello answered by a serve that failed its load")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client still waiting after serve failed its load")
	}
}
