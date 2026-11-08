package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type capturedRich struct {
	path    string
	chatID  string
	message map[string]any
	files   map[string]string
}

func richServer(t *testing.T, got *capturedRich) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.files = map[string]string{}
		var raw string
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart: %v", err)
			}
			got.chatID = r.FormValue("chat_id")
			raw = r.FormValue("rich_message")
			for name, fh := range r.MultipartForm.File {
				f, _ := fh[0].Open()
				b, _ := io.ReadAll(f)
				f.Close()
				got.files[name] = string(b)
			}
		} else {
			var body struct {
				ChatID      string          `json:"chat_id"`
				RichMessage json.RawMessage `json:"rich_message"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			got.chatID, raw = body.ChatID, string(body.RichMessage)
		}
		if err := json.Unmarshal([]byte(raw), &got.message); err != nil {
			t.Errorf("rich_message is not JSON: %v", err)
		}
		io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSendRichTextAndTable(t *testing.T) {
	var got capturedRich
	srv := richServer(t, &got)
	c, _ := NewClient(srv.Client(), srv.URL, "T", "42")

	blocks := []RichBlock{
		Heading{Text: Text("📊 MD"), Size: 2},
		Paragraph{Text: Join(Text("Total: "), Bold(Text("605")))},
		Table{Striped: true, Compact: true, Rows: [][]TableCell{
			{{Text: Text("#"), Header: true}, {Text: Text("Done"), Header: true, Align: "right"}},
			{{Text: Text("01")}, {Text: Bold(Text("+5")), Align: "right"}},
		}},
		Footer{Text: Text("Source: Intel")},
	}
	if err := c.SendRich(context.Background(), blocks); err != nil {
		t.Fatal(err)
	}
	if got.path != "/botT/sendRichMessage" || got.chatID != "42" {
		t.Fatalf("call: %s chat %s", got.path, got.chatID)
	}
	encoded, _ := json.Marshal(got.message)
	for _, want := range []string{
		`{"size":2,"text":"📊 MD","type":"heading"}`,
		`{"text":["Total: ",{"text":"605","type":"bold"}],"type":"paragraph"}`,
		`"is_compact":true`, `"is_striped":true`,
		`{"align":"left","is_header":true,"text":"#","valign":"middle"}`,
		`{"align":"right","text":{"text":"+5","type":"bold"},"valign":"middle"}`,
		`{"text":"Source: Intel","type":"footer"}`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("rich_message missing %s\n%s", want, encoded)
		}
	}
}

func TestSendRichUploadsSlideshowPhotos(t *testing.T) {
	var got capturedRich
	srv := richServer(t, &got)
	c, _ := NewClient(srv.Client(), srv.URL, "T", "-100")

	dir := t.TempDir()
	var items []Media
	for _, name := range []string{"total.png", "pace.png", "map_anim.gif"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("img-"+name), 0o600)
		items = append(items, Media{Path: p, Caption: "cap " + name, Animation: filepath.Ext(name) == ".gif"})
	}
	if err := c.SendRich(context.Background(), []RichBlock{Paragraph{Text: Text("x")}, Slideshow{Items: items}}); err != nil {
		t.Fatal(err)
	}
	if got.chatID != "-100" || got.files["media0"] != "img-total.png" || got.files["media1"] != "img-pace.png" ||
		got.files["media2"] != "img-map_anim.gif" {
		t.Fatalf("uploads: chat %q files %v", got.chatID, got.files)
	}
	encoded, _ := json.Marshal(got.message)
	for _, want := range []string{
		`"type":"slideshow"`,
		`{"caption":{"text":"cap total.png"},"photo":{"media":"attach://media0","type":"photo"},"type":"photo"}`,
		`"media":"attach://media1"`,
		`{"animation":{"media":"attach://media2","type":"animation"},"caption":{"text":"cap map_anim.gif"},"type":"animation"}`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("rich_message missing %s\n%s", want, encoded)
		}
	}
}

func TestSendRichValidates(t *testing.T) {
	c, _ := NewClient(http.DefaultClient, "http://127.0.0.1:1", "T", "1")
	ctx := context.Background()
	wide := make([]TableCell, MaxRichTableColumns+1)
	tall := Table{Rows: make([][]TableCell, MaxRichBlocks)}
	cases := map[string][]RichBlock{
		"no blocks":     nil,
		"heading size":  {Heading{Text: Text("x"), Size: 7}},
		"too wide":      {Table{Rows: [][]TableCell{wide}}},
		"too many rows": {tall},
		"missing photo": {Slideshow{Items: []Media{{Path: "/nonexistent.png"}}}},
	}
	for name, blocks := range cases {
		if err := c.SendRich(ctx, blocks); err == nil || strings.Contains(err.Error(), "connection refused") {
			t.Errorf("%s: expected validation error before any request, got %v", name, err)
		}
	}
}
