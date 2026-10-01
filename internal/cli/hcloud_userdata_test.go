package cli

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
)

func TestHetznerDesktopUserDataFitsProviderLimit(t *testing.T) {
	for _, desktop := range []string{"xfce", "wayland", "gnome"} {
		t.Run(desktop, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider, cfg.Desktop, cfg.Browser, cfg.DesktopEnv = "hetzner", true, true, desktop
			var userData string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					UserData string `json:"user_data"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				userData = body.UserData
				_, _ = io.WriteString(w, `{"server":{"id":123}}`)
			}))
			defer server.Close()
			client := HetznerClient{BaseURL: server.URL, Client: server.Client(), Token: "fixture"}
			_, err := client.CreateServer(context.Background(), cfg, "ssh-ed25519 fixture", "cbx_abcdef123456", "fixture", false)
			if err != nil {
				t.Fatal(err)
			}
			if len(userData) > 32768 {
				t.Fatalf("Hetzner user_data is %d bytes; maximum is 32768", len(userData))
			}
			message, err := mail.ReadMessage(strings.NewReader(userData))
			if err != nil {
				t.Fatal(err)
			}
			if message.Header.Get("Content-Type") != "application/gzip" || message.Header.Get("Content-Transfer-Encoding") != "base64" {
				t.Fatal("expected gzip MIME envelope")
			}
			gz, err := gzip.NewReader(base64.NewDecoder(base64.StdEncoding, message.Body))
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			decoded, err := io.ReadAll(gz)
			if err != nil {
				t.Fatal(err)
			}
			if string(decoded) != cloudInit(cfg, "ssh-ed25519 fixture") {
				t.Fatal("MIME transport changed cloud-config content")
			}
		})
	}
}
