package dell

// Real-hardware E2E for the Dell console bridge, gated by MAMMOTH_DELL_E2E:
//
//	MAMMOTH_DELL_E2E='host=192.168.1.71 user=root pass=calvin' go test ./internal/bmcweb/dell/ -run E2E
//
// The flow mirrors kvmWS minus the websocket: web login → viewer.jnlp
// ticket → avct login (APCP/TLS 5900) → video channel → framebuffer
// receives content. Read-only with respect to the machine: the console
// session is opened and closed, no power/boot operations.

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/bmcweb/avct"
)

func e2eConfig(t *testing.T) (host, user, pass string) {
	t.Helper()
	spec := os.Getenv("MAMMOTH_DELL_E2E")
	if spec == "" {
		t.Skip("MAMMOTH_DELL_E2E not set; skipping real-iDRAC E2E")
	}
	var h, u, p string
	_, err := fmt.Sscanf(spec, "host=%s user=%s pass=%s", &h, &u, &p)
	if err != nil || h == "" || u == "" {
		t.Fatalf("bad MAMMOTH_DELL_E2E spec: %q (%v) — want 'host=IP user=U pass=P'", spec, err)
	}
	return h, u, p
}

func TestE2EConsoleBridge(t *testing.T) {
	host, user, pass := e2eConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 1. Web login + console ticket.
	client, err := NewClient(host, user, pass, 60*time.Second)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := client.Login(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() {
		lctx, lcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer lcancel()
		_ = client.Logout(lctx)
	}()
	title := fmt.Sprintf("e2e, User: %s", user)
	cc, err := client.ConsoleLaunch(ctx, title)
	if err != nil {
		t.Fatalf("console launch: %v", err)
	}
	t.Logf("ticket: host=%s ports=%d/%d user=%q apcp=%v", cc.Host, cc.KMPort, cc.VPort, cc.User, cc.APCP)
	if cc.User == "" || cc.Password == "" {
		t.Fatal("empty console credentials")
	}

	// 2. avct console session on the ticket.
	sess := avct.NewSession(avct.Config{
		Host:        cc.Host,
		ControlPort: cc.KMPort,
		VideoPort:   cc.VPort,
		Username:    cc.User,
		Password:    cc.Password,
		Timeout:     45 * time.Second,
		Trace:       true,
		TraceVideo:  true,
		Logger:      avct.LogFunc(t.Logf),
	})
	if err := sess.Connect(ctx); err != nil {
		t.Fatalf("avct login: %v", err)
	}
	defer sess.Close()

	// 3. Video channel + framebuffer content.
	fb := avct.NewFramebuffer(1024, 768)
	sess.SetVideoStream(avct.NewVideoStream(fb))
	time.Sleep(500 * time.Millisecond) // let the appliance register the console session
	if err := sess.OpenVideo(ctx, fb); err != nil {
		t.Fatalf("video: %v", err)
	}
	deadline := time.Now().Add(40 * time.Second)
	var nonBlack int
	var w, h int
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if sess.State() == avct.StateRunning {
			var img *image.RGBA
			img, w, h = fb.Snapshot()
			nonBlack = 0
			for i := 0; i < len(img.Pix); i += 4 {
				if img.Pix[i] > 8 || img.Pix[i+1] > 8 || img.Pix[i+2] > 8 {
					nonBlack++
				}
			}
			total := w * h
			t.Logf("framebuffer %dx%d: %d/%d non-black pixels (%.1f%%)",
				w, h, nonBlack, total, 100*float64(nonBlack)/float64(total))
			if nonBlack > total/100 { // >1% of the surface carries content
				t.Logf("console video is live; session state %s", sess.State())
				if out := os.Getenv("MAMMOTH_DELL_E2E_PNG"); out != "" {
					f, ferr := os.Create(out)
					if ferr == nil {
						_ = png.Encode(f, img)
						_ = f.Close()
						t.Logf("framebuffer written to %s", out)
					}
				}
				return
			}
		}
	}
	t.Fatalf("no video content after deadline (state %s, last framebuffer %dx%d, nonBlack=%d)",
		sess.State(), w, h, nonBlack)
}
