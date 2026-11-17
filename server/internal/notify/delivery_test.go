package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/delivery"
)

func TestDeliveryReadyMail(t *testing.T) {
	tz := time.FixedZone("CST", 8*3600)
	p := delivery.NotifyPayload{
		DeliveryID:   uuid.New(),
		ProjectTitle: "婚礼跟拍",
		ItemCount:    120,
		Packages: []delivery.Package{
			{Spec: delivery.SpecWeb, Status: delivery.PackageReady, SizeBytes: 850 << 20, EntryCount: 120},
			{Spec: delivery.SpecOriginal, Status: delivery.PackageReady, SizeBytes: 12 << 30, EntryCount: 120},
			{Spec: "web", Status: delivery.PackageFailed},
		},
		URL:          "https://photos.example.com/#/d/token-value",
		ExpiresAt:    time.Date(2026, 11, 8, 10, 30, 0, 0, time.UTC),
		MaxDownloads: 20,
	}
	subject, body := deliveryReadyMail(p, tz)
	if !strings.Contains(subject, "婚礼跟拍") || !strings.Contains(subject, "就绪") {
		t.Fatalf("unexpected subject %q", subject)
	}
	for _, want := range []string{
		"120 张",
		"网页版（长边 2048，JPEG）：850.0 MB，120 个文件",
		"原图（与拍摄文件一致）：12.0 GB，120 个文件",
		"https://photos.example.com/#/d/token-value",
		"2026-11-08 18:30",
		"可下载 20 次",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("delivery body missing %q:\n%s", want, body)
		}
	}
	if strings.Count(body, "原图（与拍摄文件一致）") != 1 {
		t.Fatalf("failed package must not be listed:\n%s", body)
	}
}

func TestDeliveryReadyMailFallbackTitle(t *testing.T) {
	_, body := deliveryReadyMail(delivery.NotifyPayload{URL: "https://x/#/d/t"}, time.UTC)
	if !strings.Contains(body, "您的照片") {
		t.Fatalf("expected fallback title in body:\n%s", body)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:             "0 B",
		512:           "512 B",
		2048:          "2 KB",
		5 << 20:       "5.0 MB",
		3<<30 + 1<<29: "3.5 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
