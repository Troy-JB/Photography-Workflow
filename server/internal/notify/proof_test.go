package notify

import (
	"strings"
	"testing"
	"time"
)

func testMailData() *proofMailData {
	return &proofMailData{
		ClientName:   "张女士",
		ProjectTitle: "婚礼跟拍",
		GalleryTitle: "婚礼精选",
		RoundNo:      2,
		DueAt:        time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC),
		URL:          "https://photos.example.com/#/p/token-value",
	}
}

func TestInviteMail(t *testing.T) {
	subject, body := inviteMail(testMailData(), "2026-11-02 18:00")
	if !strings.Contains(subject, "婚礼跟拍") || !strings.Contains(subject, "第 2 轮") {
		t.Fatalf("unexpected subject %q", subject)
	}
	for _, want := range []string{"张女士", "https://photos.example.com/#/p/token-value", "2026-11-02 18:00", "继续复核或提出修改"} {
		if !strings.Contains(body, want) {
			t.Fatalf("invite body missing %q:\n%s", want, body)
		}
	}
}

func TestResubmittedMail(t *testing.T) {
	subject, body := resubmittedMail(testMailData())
	if !strings.Contains(subject, "修图版已回传") {
		t.Fatalf("unexpected subject %q", subject)
	}
	if !strings.Contains(body, "token-value") || !strings.Contains(body, "通过") {
		t.Fatalf("unexpected body:\n%s", body)
	}
}

func TestDeadlineMail(t *testing.T) {
	subject, body := deadlineMail(testMailData(), "2026-11-02 18:00")
	if !strings.Contains(subject, "2026-11-02 18:00") {
		t.Fatalf("unexpected subject %q", subject)
	}
	if !strings.Contains(body, "token-value") {
		t.Fatalf("deadline body lacks link:\n%s", body)
	}
}

func TestGreetingFallsBack(t *testing.T) {
	if got := greeting("  "); got != "您好" {
		t.Fatalf("expected fallback greeting, got %q", got)
	}
	if got := greeting("王先生"); got != "王先生" {
		t.Fatalf("expected name greeting, got %q", got)
	}
}

func TestMaskPhone(t *testing.T) {
	if got := maskPhone("13800138000"); got != "****8000" {
		t.Fatalf("unexpected mask %q", got)
	}
	if got := maskPhone("123"); got != "****" {
		t.Fatalf("unexpected short mask %q", got)
	}
}

func TestNewSMSProvider(t *testing.T) {
	if p, err := NewSMSProvider("", nil); err != nil || p != nil {
		t.Fatalf("empty provider should disable sms: %v %v", p, err)
	}
	p, err := NewSMSProvider("sandbox", nil)
	if err != nil || p == nil || p.Name() != SMSSandbox {
		t.Fatalf("sandbox provider: %v %v", p, err)
	}
	if _, err := NewSMSProvider("twilio", nil); err == nil {
		t.Fatal("unknown provider must fail")
	}
}
