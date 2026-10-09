package broker

import "testing"

// 版は ver 1 箇所で決まり、URL は ver と host から**組まれる**。ここが破れると
// 「ver は v4r10 なのに authBase は旧版」という混在で、認証だけ旧版へ飛ぶ。
// 版の**綴り**は TestNewTachibana_APIVersionIsPinnedToTheDefaultConst が押さえる(こちらで
// defaultTachibanaAPIVersion から組むと恒真になって何も守らない)。
func TestTachibana_AuthBaseDerivedFromVer(t *testing.T) {
	for _, env := range []string{"production", "demo", "prod", ""} {
		tb := NewTachibana(env, "authid", nil, "s", false, false, nil)
		if tb.ver == "" {
			t.Fatalf("env %q: ver is empty (版の唯一の真実源が空)", env)
		}
		if tb.APIVersion() != tb.ver {
			t.Fatalf("env %q: APIVersion() = %q, want ver %q", env, tb.APIVersion(), tb.ver)
		}
		// APIEnv は設定値**そのまま**(typo を丸めない)、APIHost は fail-close 解決**後**。
		if tb.APIEnv() != env {
			t.Fatalf("APIEnv() = %q, want the value passed to NewTachibana %q", tb.APIEnv(), env)
		}
		if tb.APIHost() != tb.host {
			t.Fatalf("env %q: APIHost() = %q, want host %q", env, tb.APIHost(), tb.host)
		}
		// authBase は host と ver から組んだもの**以外**であってはならない。
		if want := "https://" + tb.APIHost() + "/e_api_" + tb.APIVersion() + "/auth/"; tb.authBase != want {
			t.Fatalf("env %q: authBase = %q, want %q (版もホストも 1 箇所から組む)", env, tb.authBase, want)
		}
	}
}

// ver を上げたら authBase も一緒に動く(= 片方だけ直せない)ことを、
// 組み立て関数そのもので固定する。
func TestTachibanaAuthBase_FollowsVer(t *testing.T) {
	if got, want := tachibanaAuthBase("kabuka.e-shiten.jp", "v4r10"), "https://kabuka.e-shiten.jp/e_api_v4r10/auth/"; got != want {
		t.Fatalf("tachibanaAuthBase = %q, want %q", got, want)
	}
	if got, want := tachibanaAuthBase("demo-kabuka.e-shiten.jp", "v4r12"), "https://demo-kabuka.e-shiten.jp/e_api_v4r12/auth/"; got != want {
		t.Fatalf("tachibanaAuthBase must follow ver, got %q want %q", got, want)
	}
}

// 版の**綴り**を production コードの外に固定する唯一の場所。ここと
// tachibana.go の const が「切替の 2 行」(v4r10 へ切り替えた)。
// 🛑 defaultTachibanaAPIVersion から組まない(恒真になって何も守らない)。
func TestNewTachibana_APIVersionIsPinnedToTheDefaultConst(t *testing.T) {
	if got, want := NewTachibana("production", "authid", nil, "s", false, false, nil).APIVersion(), "v4r10"; got != want {
		t.Fatalf("APIVersion() = %q, want %q", got, want)
	}
}

// probe 専用の差し替え口。v4r10 を**既定版を動かさずに**実測するための唯一の経路で、
// 形式外(パスの注入・空)は fail-close、ログイン後の差し替えは拒否(仮想URL は版ごとに
// 別セッションなので「新しい版の authBase に古い版のセッション」を作れてしまう)。
func TestUseAPIVersion_RejectsGarbageAndPostLoginChange(t *testing.T) {
	tb := NewTachibana("production", "authid", nil, "s", false, false, nil)
	before := tb.authBase
	for _, bad := range []string{"", "v5", "../e_api_v4r10", "v4r10/auth", "V4R10", "v4r123"} {
		if err := tb.UseAPIVersion(bad); err == nil {
			t.Fatalf("UseAPIVersion(%q) must be rejected", bad)
		}
		if tb.authBase != before || tb.APIVersion() != defaultTachibanaAPIVersion {
			t.Fatalf("rejected UseAPIVersion(%q) must not change state: authBase=%q ver=%q", bad, tb.authBase, tb.APIVersion())
		}
	}
	if err := tb.UseAPIVersion("v4r10"); err != nil {
		t.Fatalf("UseAPIVersion(v4r10): %v", err)
	}
	if got, want := tb.APIVersion(), "v4r10"; got != want {
		t.Fatalf("APIVersion() = %q, want %q", got, want)
	}
	if got, want := tb.authBase, "https://kabuka.e-shiten.jp/e_api_v4r10/auth/"; got != want {
		t.Fatalf("authBase = %q, want %q (版とホストから組み直す)", got, want)
	}
	// ログイン後は差し替えられない。
	tb.mu.Lock()
	tb.session = &tachiSession{requestURL: "http://127.0.0.1:1/req", lastNo: 1}
	tb.mu.Unlock()
	if err := tb.UseAPIVersion("v4r10"); err == nil {
		t.Fatal("UseAPIVersion after login must be rejected")
	}
}
