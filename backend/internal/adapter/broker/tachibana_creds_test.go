package broker

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"testing"

	"os"
)

func testPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func setCredEnv(t *testing.T, authID, keyPEM, keyFile, secondPW string) {
	t.Helper()
	t.Setenv("STOCKBOT_TACHIBANA_AUTH_ID", authID)
	t.Setenv("STOCKBOT_TACHIBANA_PRIVATE_KEY", keyPEM)
	t.Setenv("STOCKBOT_TACHIBANA_PRIVATE_KEY_FILE", keyFile)
	t.Setenv("STOCKBOT_TACHIBANA_SECOND_PASSWORD", secondPW)
}

func TestTachibanaCredsFromEnv_InlineAndFile(t *testing.T) {
	p := testPEM(t)

	setCredEnv(t, "u1", p, "", "pw")
	c, err := TachibanaCredsFromEnv(false)
	if err != nil || c.AuthID != "u1" || c.Key == nil || c.SecondPW != "pw" {
		t.Fatalf("inline PEM: %+v err=%v", c, err)
	}

	// _FILE はインライン PEM より優先する(運用は file 経由)。
	dir := t.TempDir()
	fp := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(fp, []byte(p), 0o600); err != nil {
		t.Fatal(err)
	}
	setCredEnv(t, "u2", "not-a-pem", fp, "")
	c, err = TachibanaCredsFromEnv(false)
	if err != nil || c.AuthID != "u2" || c.Key == nil {
		t.Fatalf("file PEM が優先されていない: %+v err=%v", c, err)
	}
}

func TestTachibanaCredsFromEnv_FailsClosed(t *testing.T) {
	p := testPEM(t)
	for _, tc := range []struct {
		name                              string
		authID, keyPEM, keyFile, secondPW string
		needSecondPW                      bool
	}{
		{name: "authID 欠落", keyPEM: p},
		{name: "鍵欠落", authID: "u"},
		{name: "第二暗証番号が必要なのに欠落", authID: "u", keyPEM: p, needSecondPW: true},
		{name: "鍵ファイルが読めない", authID: "u", keyFile: "/nonexistent/key.pem"},
		{name: "PEM が壊れている", authID: "u", keyPEM: "garbage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCredEnv(t, tc.authID, tc.keyPEM, tc.keyFile, tc.secondPW)
			if _, err := TachibanaCredsFromEnv(tc.needSecondPW); err == nil {
				t.Fatal("fail-close していない(認証material 不備で error を返すこと)")
			}
		})
	}
}
