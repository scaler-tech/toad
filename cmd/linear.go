package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/scaler-tech/toad/internal/linearauth"
	"github.com/scaler-tech/toad/internal/state"
)

const connectCallbackAddr = "localhost:9482"

var linearCmd = &cobra.Command{
	Use:   "linear",
	Short: "Linear app-identity commands",
}

var linearConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect toad's Linear OAuth app (actor=app) so tickets come from the app, not you",
	RunE:  runLinearConnect,
}

func init() {
	linearCmd.AddCommand(linearConnectCmd)
	rootCmd.AddCommand(linearCmd)
}

func runLinearConnect(cmd *cobra.Command, args []string) error {
	clientID := os.Getenv("TOAD_LINEAR_CLIENT_ID")
	clientSecret := os.Getenv("TOAD_LINEAR_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return errors.New("set TOAD_LINEAR_CLIENT_ID and TOAD_LINEAR_CLIENT_SECRET first (create the OAuth app in Linear workspace settings: redirect URL http://localhost:9482/callback, agent capabilities enabled, and webhooks ENABLED with agent session events — Linear requires webhooks on for agent sessions to be created at all; any reachable-looking URL works, toad polls and never reads the deliveries)")
	}

	db, err := state.OpenDB()
	if err != nil {
		return fmt.Errorf("opening state db: %w", err)
	}
	defer db.Close()

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return err
	}
	oauthState := hex.EncodeToString(stateBytes)
	redirectURI := "http://" + connectCallbackAddr + "/callback"

	codeCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		runConnectCallback(w, r, oauthState, codeCh)
	})
	srv := &http.Server{Addr: connectCallbackAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "callback server: %v\n", err)
		}
	}()
	defer srv.Shutdown(context.Background())

	authURL := linearauth.BuildAuthorizeURL(clientID, redirectURI, oauthState)
	fmt.Println("Opening Linear to authorize toad as a workspace app (admin required).")
	fmt.Println("If the browser does not open, visit:\n\n  " + authURL + "\n")
	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(5 * time.Minute):
		return errors.New("timed out waiting for the OAuth callback (5m)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tr, err := linearauth.Exchange(ctx, http.DefaultClient, linearauth.DefaultTokenURL, clientID, clientSecret, code, redirectURI)
	if err != nil {
		return fmt.Errorf("exchanging authorization code: %w", err)
	}
	store := linearauth.NewStore(db)
	if err := store.SaveTokens(tr.AccessToken, tr.RefreshToken); err != nil {
		return fmt.Errorf("storing tokens: %w", err)
	}

	if name := viewerName(ctx, tr.AccessToken); name != "" {
		fmt.Printf("Connected to Linear as app identity %q. Tickets and comments now come from the app.\n", name)
	} else {
		fmt.Println("Connected to Linear as the app identity. Tickets and comments now come from the app.")
	}
	fmt.Println("If the toad daemon is running, restart it (toad restart) to switch to the app identity and start the Linear agent poller.")
	return nil
}

// runConnectCallback validates the OAuth redirect and hands the code to the
// connect flow. Split out for testing.
func runConnectCallback(w http.ResponseWriter, r *http.Request, wantState string, codeCh chan<- string) {
	if r.URL.Query().Get("state") != wantState {
		http.Error(w, "state mismatch — restart toad linear connect", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	_, _ = fmt.Fprintln(w, "Toad is connected to Linear. You can close this tab.")
	select {
	case codeCh <- code:
	default:
	}
}

// viewerName asks Linear who the new token authenticates as (best-effort).
func viewerName(ctx context.Context, token string) string {
	body := []byte(`{"query":"{ viewer { name } }"}`)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.linear.app/graphql", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Viewer struct {
				Name string `json:"name"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	return out.Data.Viewer.Name
}
