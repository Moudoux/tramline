package secure

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var loopback = net.IPv4(127, 0, 0, 1)

func fp(c string) string { return strings.Repeat(c, 64) }

func TestInitAndKeyGenRefuseToOverwrite(t *testing.T) {
	dir := t.TempDir()
	first, err := Init(dir, loopback)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, loopback); err == nil {
		t.Fatal("expected Init to refuse")
	}
	if got, err := ServerFingerprint(dir); err != nil || got != first {
		t.Fatalf("got %q, %v, want %q", got, err, first)
	}
	if _, err := KeyGen(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := KeyGen(dir); err == nil {
		t.Fatal("expected KeyGen to refuse")
	}
}

func TestPrivateKeysAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, loopback); err != nil {
		t.Fatal(err)
	}
	if _, err := KeyGen(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{serverKey, clientKey} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", name, info.Mode().Perm())
		}
	}
}

func TestAllowAndRevoke(t *testing.T) {
	dir := t.TempDir()
	if Allowed(dir, fp("a")) {
		t.Fatal("allowed with no list")
	}
	if err := Allow(dir, strings.ToUpper(fp("a")), "this machine"); err != nil {
		t.Fatal(err)
	}
	if err := Allow(dir, fp("b"), "laptop"); err != nil {
		t.Fatal(err)
	}
	if !Allowed(dir, fp("a")) || !Allowed(dir, fp("b")) || Allowed(dir, fp("c")) {
		t.Fatal("wrong allow results")
	}
	clients, err := Clients(dir)
	if err != nil || len(clients) != 2 || clients[0].Name != "this-machine" {
		t.Fatalf("got %+v, %v", clients, err)
	}

	if err := Allow(dir, fp("a"), "renamed"); err != nil {
		t.Fatal(err)
	}
	if clients, _ := Clients(dir); len(clients) != 2 {
		t.Fatalf("allowing twice added a duplicate: %+v", clients)
	}

	if err := Revoke(dir, fp("a")); err != nil {
		t.Fatal(err)
	}
	if Allowed(dir, fp("a")) || !Allowed(dir, fp("b")) {
		t.Fatal("wrong client revoked")
	}
	if err := Revoke(dir, fp("a")); err == nil {
		t.Fatal("expected an error revoking an unknown client")
	}
}

func TestAllowRejectsBadFingerprints(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", "abc", fp("g"), fp("a") + "a"} {
		if err := Allow(dir, bad, "x"); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// A corrupted allow list must lock everyone out, not let everyone in.
func TestMalformedListDeniesEveryone(t *testing.T) {
	dir := t.TempDir()
	if err := Allow(dir, fp("a"), "x"); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, allowFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not a fingerprint\n")
	f.Close()
	if Allowed(dir, fp("a")) {
		t.Fatal("allowed despite a malformed list")
	}
}
