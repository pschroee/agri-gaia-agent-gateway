// Orchestrator of the PoC (stage 1): warm pool with pi sandboxes, chats in
// Postgres, artifacts in RustFS, API and web UI, LLM proxy for the sandboxes.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"agw/internal/api"
	"agw/internal/artifacts"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/llmproxy"
	"agw/internal/oidc"
	"agw/internal/platform"
	"agw/internal/pool"
	"agw/internal/sandbox"
	"agw/internal/store"
	"agw/internal/titler"
	"agw/internal/webproxy"
	"agw/internal/worker"
	"agw/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := run(); err != nil {
		slog.Error("orchestrator exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	env := config.FromEnv()
	cat, err := config.LoadCatalog(env.CatalogPath)
	if err != nil {
		return err
	}
	if env.DefaultModel != "" {
		if _, _, ok := cat.Lookup(env.DefaultModel); !ok {
			return errors.New("AGW_DEFAULT_MODEL not in the catalogue: " + env.DefaultModel)
		}
		cat.Default = env.DefaultModel
	}
	for _, p := range cat.Providers {
		if p.APIKey() == "" {
			slog.Warn("no key set for provider", "provider", p.ID, "variable", p.APIKeyEnv)
		}
	}
	if err := env.CheckAuth(); err != nil {
		return err
	}
	var auth *oidc.Service
	if env.AuthMode == config.AuthOIDC {
		if auth, err = oidc.New(oidc.Config{Issuer: env.OIDCIssuer, ClientID: env.OIDCClientID, ClientSecret: env.OIDCClientSecret, PublicURL: env.PublicURL}); err != nil {
			return err
		}
		// The UI is opened under the public address; its host is therefore allowed.
		if h := env.PublicHost(); h != "" && !slices.Contains(env.AllowedHosts, h) {
			env.AllowedHosts = append(env.AllowedHosts, h)
		}
		slog.Info("login through the platform (OIDC)", "issuer", env.OIDCIssuer, "client", env.OIDCClientID, "redirect", env.PublicURL+oidc.CallbackPath, "base", env.BasePath+"/", "embedding", env.FrameAncestors)
	}
	blocked, err := api.ParseSubnets(env.BlockedSubnets)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, env.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	blobs, err := artifacts.NewS3(ctx, env.S3Endpoint, env.S3AccessKey, env.S3SecretKey, env.S3Bucket)
	if err != nil {
		return err
	}

	rt, err := sandbox.New(env.EgressNetwork)
	if err != nil {
		return err
	}
	if err := rt.EnsureEgressNetwork(ctx, env.EgressSubnet); err != nil {
		return err
	}
	self := os.Getenv("AGW_SELF_CONTAINER")
	if self == "" {
		self, _ = os.Hostname() // in a container: the short container ID
	}
	rt.SetSelf(self)
	rt.SetPkgCaches(env.NpmCache, env.PipCache)
	if n, err := rt.RemoveManaged(ctx); err == nil && n > 0 {
		slog.Info("removed sandboxes of an earlier run", "count", n)
	}
	if n := rt.RemoveSlotNetworks(ctx); n > 0 {
		slog.Info("removed slot networks of an earlier run", "count", n)
	}
	fac, err := worker.NewFactory(rt, cat, env)
	if err != nil {
		return err
	}
	pcfg := platform.Config{APIURL: env.PlatformAPIURL, TokenURL: env.PlatformTokenURL,
		ClientID: env.PlatformClientID, ClientSecret: env.PlatformClientSecret, User: env.PlatformUser, Password: env.PlatformPassword,
		Exchange: env.PlatformExchange, Audiences: env.PlatformAudiences}
	konto := env.PlatformUser
	if auth != nil {
		// Every chat acts for its owner: the owner's token from the session is the subject_token.
		pcfg.Subject = ownerToken(st, auth)
		pcfg.User, pcfg.Password, konto = "", "", "owner of the chat"
		if env.PlatformAPIURL != "" && !env.PlatformExchange {
			slog.Warn("login through the platform without token exchange: the user's token goes to the API unchanged (AGW_PLATFORM_TOKEN_EXCHANGE=true recommended)")
		}
	}
	plat, err := platform.New(pcfg)
	if err != nil {
		return err
	}
	if plat == nil {
		slog.Info("platform binding off (AGW_PLATFORM_API_URL empty)")
	} else {
		slog.Info("platform binding on", "api", env.PlatformAPIURL, "account", konto, "client", env.PlatformClientID, "token_exchange", plat.Exchanging())
	}
	p := pool.New[chat.Agent](fac.Create, fac.Destroy, env.PoolSizes)
	m := chat.NewManager(st, p, cat, blobs, artifacts.NewBroker(), chat.Options{
		IdleTimeout: env.IdleTimeout, ApprovalTimeout: env.ApprovalTimeout,
		ArtifactMaxBytes: env.ArtifactMaxBytes, InternetDefault: env.InternetDefault, ImageMaxBytes: env.ImageMaxBytes,
		WorkspaceMaxBytes:  env.WorkspaceMaxBytes,
		MaxSubagents:       env.MaxSubagents,
		AutoCompactDefault: env.AutoCompactDefault, CompactReserveTokens: env.CompactReserveTokens, CompactKeepRecent: env.CompactKeepRecent,
		BgWakesPerHour: env.BgWakesPerHour, BgKeepAlive: env.BgKeepAlive, AutoTurnsMax: env.AutoTurnsMax,
		Titler: newTitler(cat, env.TitleModel), Platform: plat,
	})
	fac.Backend = m
	plat.SetOnExchange(m.PlatformExchanged)
	if err := m.Recover(ctx); err != nil {
		return err
	}
	poolCtx, poolCancel := context.WithCancel(context.Background())
	p.Start(poolCtx)

	var webFS fs.FS
	if sub, err := fs.Sub(web.Dist, "dist"); err == nil {
		if _, err := fs.Stat(sub, "index.html"); err == nil {
			webFS = sub
		}
	}
	if webFS == nil {
		slog.Warn("web UI not built; only the API is reachable")
	}
	// All requests hang off baseCtx; on exit this also closes open SSE
	// connections, which would otherwise hold up Shutdown until the deadline.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	apiSrv := &http.Server{Addr: env.HTTPAddr, Handler: (&api.Server{M: m, Pool: p, Cat: cat, Env: env, Web: webFS, Blocked: blocked, Token: env.APIToken, AllowedHosts: env.AllowedHosts,
		OIDC: auth, FrameAncestors: env.FrameAncestors, Platform: plat}).Handler(),
		ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return baseCtx }}
	proxy := llmproxy.New(cat)
	proxy.SetRecorder(m) // assignment per slot, billing, hard limit of concurrent agents
	proxySrv := &http.Server{Addr: env.ProxyAddr, Handler: proxy,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
	// Web proxy for web_search and web_extract from pi's container (only with internet).
	web := &webproxy.Proxy{Gate: m, Blocked: blocked}
	if env.SearxURL != "" {
		if u, err := url.Parse(env.SearxURL); err == nil && u.Host != "" {
			web.Searx = u
		}
	}
	webSrv := &http.Server{Addr: env.WebProxyAddr, Handler: web, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10}

	errc := make(chan error, 3)
	go func() { errc <- apiSrv.ListenAndServe() }()
	go func() { errc <- proxySrv.ListenAndServe() }()
	go func() { errc <- webSrv.ListenAndServe() }()
	slog.Info("orchestrator running", "api", env.HTTPAddr, "proxy", env.ProxyAddr, "pool", env.PoolSizes, "model", cat.Default, "image", env.Image)

	select {
	case <-ctx.Done():
		slog.Info("shutting down: saving sessions and removing sandboxes")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
		}
	}
	// First save the chats (with their own deadline), then stop the servers
	// (Review M6).
	mctx, mcancel := context.WithTimeout(context.Background(), 20*time.Second)
	m.Shutdown(mctx)
	mcancel()
	cancelBase()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = apiSrv.Shutdown(sctx)
	poolCancel()
	p.Shutdown(sctx)
	_ = proxySrv.Shutdown(sctx)
	_ = webSrv.Shutdown(sctx)
	return nil
}

// ownerToken returns, per chat, the access token of its owner from the owner's session at the orchestrator.
// Without a live session the platform call fails with oidc.ErrNoSession (message to the agent).
func ownerToken(st *store.Store, auth *oidc.Service) func(ctx context.Context, chatID string) (string, error) {
	return func(ctx context.Context, chatID string) (string, error) {
		owner, err := st.ChatOwner(ctx, chatID)
		if err != nil {
			return "", err
		}
		if owner == "" {
			return "", errors.New("chat without owner (created in token mode); create a new chat for the platform")
		}
		return auth.AccessToken(ctx, owner)
	}
}

// newTitler: chat titles from the chat's model or from AGW_TITLE_MODEL; "off" switches them off.
func newTitler(cat *config.Catalog, model string) chat.Titler {
	if model == "off" {
		return nil
	}
	return titler.New(cat, model)
}
