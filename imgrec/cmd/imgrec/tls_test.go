package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slopbooru/imgrec/internal/api"
	"slopbooru/imgrec/internal/auth"
)

// The API over TLS with a certificate from `imgrec cert`: submit trusts it
// with --cacert and refuses it without; replacing the files swaps the
// certificate without a restart; a half-written pair keeps the old one.
func TestTLS(t *testing.T) {
	dir := t.TempDir()
	pictures(t, filepath.Join(dir, "pics"))
	t.Setenv("IMGREC_STUB_LOG", filepath.Join(dir, "stub.log"))
	f := fleet{db: filepath.Join(dir, "results.db"), specsDir: "../../specs", manifest: manifestFile(t, dir),
		vllm: stub, pixai: stub + " pixai-worker", logs: filepath.Join(dir, "logs"),
		rotation: 2 * time.Hour, maxAttempts: 3}
	p, sc, closeAll, err := f.open()
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	sc.Logf = t.Logf
	a, _ := auth.Open(p.Store.DB)
	alice, _ := a.Add("alice", false)

	crt, key := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	if err := certCmd([]string{"--hosts", "127.0.0.1,localhost", "--cert", crt, "--key", key}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(key); st.Mode().Perm() != 0o600 {
		t.Errorf("the key is %v, want 0600", st.Mode().Perm())
	}
	if err := certCmd([]string{"--hosts", "x", "--cert", crt, "--key", key}); err == nil {
		t.Error("imgrec cert overwrote an existing key")
	}
	if err := certCmd([]string{"--hosts", "x", "--cert", crt, "--key", key, "--if-missing"}); err != nil {
		t.Errorf("--if-missing with the certificate there: %v", err)
	}

	certs, err := newCertReloader(crt, key, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	// as serve does it (httptest would put its own certificate first)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: &api.Server{P: p, Auth: a, Sched: sc, MaxUpload: 64 << 20, MaxWait: 30 * time.Second},
		TLSConfig: certs.config()}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()
	ts := struct{ URL string }{"https://" + ln.Addr().String()}
	go sc.Run(t.Context(), false)

	pic := filepath.Join(dir, "pics", "p00.png")
	submit := func(cacert string) error {
		args := []string{"--server", ts.URL, "--token", alice, "--models", "pixai", "--wait", "20"}
		if cacert != "" {
			args = append(args, "--cacert", cacert)
		}
		return submitCmd(append(args, pic))
	}
	if !strings.HasPrefix(ts.URL, "https://") {
		t.Fatalf("not https: %s", ts.URL)
	}
	if err := submit(""); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("without --cacert: %v, want a certificate error", err)
	}
	if err := submit(crt); err != nil {
		t.Fatalf("with --cacert: %v", err)
	}

	// a renewal: new files in place, the next connection gets them
	time.Sleep(10 * time.Millisecond) // a different mtime
	os.Remove(crt)
	os.Remove(key)
	if err := certCmd([]string{"--hosts", "127.0.0.1", "--cert", crt + ".new", "--key", key}); err != nil {
		t.Fatal(err)
	}
	os.Rename(crt+".new", crt)
	// trusting only the new certificate: the server must be serving it
	if err := submit(crt); err != nil {
		t.Errorf("after the renewal, with the new certificate: %v", err)
	}
	// half-written: the file truncated -- the pair already loaded stays
	renewed := filepath.Join(dir, "renewed.crt")
	data, _ := os.ReadFile(crt)
	os.WriteFile(renewed, data, 0o644)
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(crt, data[:len(data)/2], 0o644)
	if err := submit(renewed); err != nil {
		t.Errorf("with a half-written certificate on disk: %v (the loaded one should stay)", err)
	}
}

func TestCleartextRemote(t *testing.T) {
	for server, want := range map[string]bool{
		"http://127.0.0.1:8100": false, "http://localhost:8100": false, "http://[::1]:8100": false,
		"https://library:8100": false, "http://library:8100": true, "http://10.0.0.2:8100": true,
		"http://0.0.0.0:8100": true,
	} {
		if got := cleartextRemote(server); got != want {
			t.Errorf("cleartextRemote(%s) = %v, want %v", server, got, want)
		}
	}
}
