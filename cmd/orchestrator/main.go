// Orchestrator des PoC (Stufe 1): Warm-Pool mit pi-Sandboxen, Chats in
// Postgres, Artefakte in RustFS, API und Web-UI, LLM-Proxy für die Sandboxen.
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
		slog.Error("Orchestrator beendet mit Fehler", "fehler", err)
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
			return errors.New("AGW_DEFAULT_MODEL nicht im Katalog: " + env.DefaultModel)
		}
		cat.Default = env.DefaultModel
	}
	for _, p := range cat.Providers {
		if p.APIKey() == "" {
			slog.Warn("kein Schlüssel für Anbieter gesetzt", "anbieter", p.ID, "variable", p.APIKeyEnv)
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
		// Die UI wird unter der öffentlichen Adresse aufgerufen; ihr Host gilt damit als erlaubt.
		if h := env.PublicHost(); h != "" && !slices.Contains(env.AllowedHosts, h) {
			env.AllowedHosts = append(env.AllowedHosts, h)
		}
		slog.Info("Anmeldung über die Plattform (OIDC)", "issuer", env.OIDCIssuer, "client", env.OIDCClientID, "redirect", env.PublicURL+oidc.CallbackPath, "basis", env.BasePath+"/", "einbettung", env.FrameAncestors)
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
		self, _ = os.Hostname() // im Container: die kurze Container-ID
	}
	rt.SetSelf(self)
	rt.SetPkgCaches(env.NpmCache, env.PipCache)
	if n, err := rt.RemoveManaged(ctx); err == nil && n > 0 {
		slog.Info("Sandboxen eines früheren Laufs entfernt", "anzahl", n)
	}
	if n := rt.RemoveSlotNetworks(ctx); n > 0 {
		slog.Info("Platz-Netze eines früheren Laufs entfernt", "anzahl", n)
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
		// Jeder Chat handelt für seinen Besitzer: dessen Token aus der Sitzung ist das subject_token.
		pcfg.Subject = ownerToken(st, auth)
		pcfg.User, pcfg.Password, konto = "", "", "Besitzer des Chats"
		if env.PlatformAPIURL != "" && !env.PlatformExchange {
			slog.Warn("Anmeldung über die Plattform ohne Token-Austausch: das Token des Nutzers geht unverändert an die API (AGW_PLATFORM_TOKEN_EXCHANGE=true empfohlen)")
		}
	}
	plat, err := platform.New(pcfg)
	if err != nil {
		return err
	}
	if plat == nil {
		slog.Info("Plattform-Anbindung aus (AGW_PLATFORM_API_URL leer)")
	} else {
		slog.Info("Plattform-Anbindung an", "api", env.PlatformAPIURL, "konto", konto, "client", env.PlatformClientID, "token_austausch", plat.Exchanging())
	}
	p := pool.New[chat.Agent](fac.Create, fac.Destroy, env.PoolSizes)
	m := chat.NewManager(st, p, cat, blobs, artifacts.NewBroker(), chat.Options{
		IdleTimeout: env.IdleTimeout, ApprovalTimeout: env.ApprovalTimeout,
		ArtifactMaxBytes: env.ArtifactMaxBytes, InternetDefault: env.InternetDefault, ImageMaxBytes: env.ImageMaxBytes,
		WorkspaceMaxBytes:   env.WorkspaceMaxBytes,
		MaxSubagentsDefault: env.MaxSubagentsDefault, MaxSubagentsLimit: env.MaxSubagentsLimit,
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
		slog.Warn("Web-UI nicht gebaut; nur die API ist erreichbar")
	}
	// Alle Anfragen hängen an baseCtx; beim Beenden werden damit auch offene
	// SSE-Verbindungen geschlossen, die Shutdown sonst bis zur Frist aufhielten.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	apiSrv := &http.Server{Addr: env.HTTPAddr, Handler: (&api.Server{M: m, Pool: p, Cat: cat, Env: env, Web: webFS, Blocked: blocked, Token: env.APIToken, AllowedHosts: env.AllowedHosts,
		OIDC: auth, FrameAncestors: env.FrameAncestors}).Handler(),
		ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return baseCtx }}
	proxy := llmproxy.New(cat)
	proxy.SetRecorder(m) // Zuordnung je Platz, Abrechnung, harte Grenze gleichzeitiger Agenten
	proxySrv := &http.Server{Addr: env.ProxyAddr, Handler: proxy,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
	// Web-Proxy für web_search und web_extract aus dem Container von pi (nur mit Internet).
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
	slog.Info("Orchestrator läuft", "api", env.HTTPAddr, "proxy", env.ProxyAddr, "pool", env.PoolSizes, "modell", cat.Default, "abbild", env.Image)

	select {
	case <-ctx.Done():
		slog.Info("Beende: sichere Sitzungen und baue Sandboxen ab")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Server-Fehler", "fehler", err)
		}
	}
	// Zuerst die Chats sichern (mit eigener Frist), dann die Server beenden
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

// ownerToken liefert je Chat das Zugangstoken seines Besitzers aus dessen Sitzung am Orchestrator.
// Ohne lebende Sitzung scheitert der Plattform-Aufruf mit oidc.ErrNoSession (Meldung an den Agenten).
func ownerToken(st *store.Store, auth *oidc.Service) func(ctx context.Context, chatID string) (string, error) {
	return func(ctx context.Context, chatID string) (string, error) {
		owner, err := st.ChatOwner(ctx, chatID)
		if err != nil {
			return "", err
		}
		if owner == "" {
			return "", errors.New("Chat ohne Besitzer (angelegt im token-Modus); für die Plattform einen neuen Chat anlegen")
		}
		return auth.AccessToken(ctx, owner)
	}
}

// newTitler: Chattitel vom Modell des Chats oder von AGW_TITLE_MODEL; "off" schaltet sie ab.
func newTitler(cat *config.Catalog, model string) chat.Titler {
	if model == "off" {
		return nil
	}
	return titler.New(cat, model)
}
