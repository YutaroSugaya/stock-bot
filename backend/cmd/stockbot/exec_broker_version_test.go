package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
)

// 起動ログに「いま叩いている API 版」が 1 行出ること。v4r9 廃止の
// 移行中は、版がログから読めないと「本番だけ旧版のまま」が黙って走る。
func TestLogBrokerAPIVersion(t *testing.T) {
	logged := func(set brokerSet) string {
		var buf bytes.Buffer
		logBrokerAPIVersion(slog.New(slog.NewJSONHandler(&buf, nil)), set)
		return buf.String()
	}

	tb := broker.NewTachibana("production", "authid", nil, "s", false, false, nil)
	out := logged(brokerSet{tb: tb})
	// 版の綴りは tachibana_version_test.go が押さえる。ここでは literal を書かない
	// (書くと受入 A2t の allowlist の外に v4r9 が増える)。
	for _, want := range []string{`"ver":"` + tb.APIVersion() + `"`, `"env":"production"`, `"host":"kabuka.e-shiten.jp"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("startup log %q missing %s", out, want)
		}
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("want exactly 1 log line, got %d: %q", n, out)
	}

	// 🛑 typo は丸めない: env はそのまま、host は fail-close 解決後。同じ 1 行で読める。
	out = logged(brokerSet{tb: broker.NewTachibana("prod", "authid", nil, "s", false, false, nil)})
	for _, want := range []string{`"env":"prod"`, `"host":"demo-kabuka.e-shiten.jp"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("typo'd env: startup log %q missing %s", out, want)
		}
	}

	// paper 構成(実ブローカー無し)では何も出さない。
	if out := logged(brokerSet{}); out != "" {
		t.Fatalf("paper 構成では版ログを出さない, got %q", out)
	}
	// logger が無い経路で panic しない(起動前段の呼び出しを塞ぐ)。
	logBrokerAPIVersion(nil, brokerSet{})
}

// 版更新告知(login 応答の sUpdateInformAPISpecFunction / sUpdateInformWebDocument)。
// 今回の移行も bot は毎日ログインしながら一言も言わなかった。判定は v4r10 リファレンス
// v10:231「(予定日 ≧ 当日) AND (予定日 != 前回受信値・初回は空白)」。
// 🛑 fail-close にしない — 告知が出た日に bot が起動しなくなる方が危険なので Warn だけ。
func TestWarnAPIUpdateNotice(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state", "tachibana-update-notice.json")
	now := time.Date(2026, 9, 4, 19, 0, 0, 0, time.FixedZone("JST", 9*3600))
	run := func(n broker.APIUpdateNotice) string {
		var buf bytes.Buffer
		warnAPIUpdateNotice(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), n, statePath, now)
		return buf.String()
	}

	// 初回(前回値なし)・予定日=当日 → 鳴る。両項目とも独立に判定する。
	out := run(broker.APIUpdateNotice{APISpecFunction: "20260904", WebDocument: "20261001"})
	if !strings.Contains(out, `"planned":"20260904"`) || !strings.Contains(out, `"planned":"20261001"`) {
		t.Fatalf("first sight of a future notice must warn for both fields, got %q", out)
	}
	if n := strings.Count(out, "\n"); n != 2 {
		t.Fatalf("want 2 warn lines (one per field), got %d: %q", n, out)
	}
	// 前回値を保存している(同じ告知で毎日鳴らない)。
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	var st map[string]string
	if err := json.Unmarshal(b, &st); err != nil || st["api_spec_function"] != "20260904" || st["web_document"] != "20261001" {
		t.Fatalf("state file content wrong: %s (err=%v)", b, err)
	}
	if out := run(broker.APIUpdateNotice{APISpecFunction: "20260904", WebDocument: "20261001"}); out != "" {
		t.Fatalf("same notice must not warn twice, got %q", out)
	}
	// 予定日が動いたら再び鳴る(片方だけ)。
	out = run(broker.APIUpdateNotice{APISpecFunction: "20260927", WebDocument: "20261001"})
	if !strings.Contains(out, `"planned":"20260927"`) || strings.Contains(out, `"planned":"20261001"`) {
		t.Fatalf("only the changed field must warn, got %q", out)
	}
	// 過去日 / 空 / "0" は鳴らない・error にもならない(fail-close にしない)。
	for _, quiet := range []broker.APIUpdateNotice{
		{APISpecFunction: "20260903"}, {APISpecFunction: ""}, {APISpecFunction: "0"}, {APISpecFunction: "garbage"},
	} {
		if out := run(quiet); out != "" {
			t.Fatalf("notice %+v must be silent, got %q", quiet, out)
		}
	}
	// logger 無し / 書けないパスで panic しない。
	warnAPIUpdateNotice(nil, broker.APIUpdateNotice{APISpecFunction: "20991231"}, statePath, now)
	warnAPIUpdateNotice(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), broker.APIUpdateNotice{APISpecFunction: "20991231"}, filepath.Join(statePath, "not-a-dir", "x.json"), now)
}

// paper 構成(実ブローカー無し)では告知を読まない。
func TestBrokerAPIUpdateNotice_PaperHasNone(t *testing.T) {
	if _, ok := brokerAPIUpdateNotice(brokerSet{}); ok {
		t.Fatal("paper 構成に告知は無い")
	}
	if _, ok := brokerAPIUpdateNotice(brokerSet{tb: broker.NewTachibana("demo", "a", nil, "s", false, false, nil)}); !ok {
		t.Fatal("立花が挿さっていれば(未 login でも)告知の器は取れる")
	}
}
