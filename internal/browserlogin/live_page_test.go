package browserlogin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// LIVE=1 LITREVIEW_PAGE=http://localhost:8080/projects/<id>/keywords go test ./internal/browserlogin -run LiveKeyword -v
// Loads the keyword-map page in headless Chromium and checks that vis-network
// actually drew a canvas and that no script errors occurred.
func TestLiveKeywordMapRenders(t *testing.T) {
	page := os.Getenv("LITREVIEW_PAGE")
	if os.Getenv("LIVE") == "" || page == "" {
		t.Skip("LIVE and LITREVIEW_PAGE not set")
	}
	bin, err := FindBrowser("")
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := Launch(ctx, bin, filepath.Join(t.TempDir(), "profile"), "--headless=new", "--disable-gpu", "--window-size=1280,900", "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var tgt struct {
		TargetID string `json:"targetId"`
	}
	if err := c.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &tgt); err != nil {
		t.Fatal(err)
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": tgt.TargetID, "flatten": true}, &att); err != nil {
		t.Fatal(err)
	}
	sid := att.SessionID
	_ = c.Call(ctx, sid, "Page.enable", map[string]any{}, nil)
	_ = c.Call(ctx, sid, "Runtime.enable", map[string]any{}, nil)
	go func() { _ = c.Call(ctx, sid, "Page.navigate", map[string]any{"url": page}, nil) }()
	var errors []string
	deadline := time.After(14 * time.Second)
loop:
	for {
		select {
		case ev := <-c.Events():
			if ev.Method == "Runtime.exceptionThrown" {
				var p struct {
					ExceptionDetails struct {
						Text      string `json:"text"`
						Exception struct {
							Description string `json:"description"`
						} `json:"exception"`
					} `json:"exceptionDetails"`
				}
				_ = json.Unmarshal(ev.Params, &p)
				errors = append(errors, p.ExceptionDetails.Text+" "+p.ExceptionDetails.Exception.Description)
			}
		case <-deadline:
			break loop
		}
	}
	var res struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	expr := `JSON.stringify({canvases: document.querySelectorAll('#map canvas').length, visLoaded: typeof vis !== 'undefined', nodes: (window.__net ? window.__net.nodes.length : -1), frozen: window.__frozen === true, physics: (window.__network ? window.__network.physics.options.enabled : null), title: document.title})`
	if err := c.Call(ctx, sid, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}, &res); err != nil {
		t.Fatal(err)
	}
	t.Logf("page state: %s", res.Result.Value)
	if len(errors) > 0 {
		t.Fatalf("script errors: %s", strings.Join(errors, " | "))
	}
	if !strings.Contains(res.Result.Value, `"canvases":1`) || !strings.Contains(res.Result.Value, `"visLoaded":true`) {
		t.Fatalf("map not drawn: %s", res.Result.Value)
	}
	if !strings.Contains(res.Result.Value, `"frozen":true`) || !strings.Contains(res.Result.Value, `"physics":false`) {
		t.Fatalf("layout did not freeze after stabilisation: %s", res.Result.Value)
	}
}
