// Orchestrator of the PoC (stage 1): warm pool with pi sandboxes, chats in
// Postgres, artifacts in RustFS, API and web UI, LLM proxy for the sandboxes.
package main

import (
	"context"
	"errors"
	"fmt"
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

	"agw/internal/annotation"
	"agw/internal/api"
	"agw/internal/artifacts"
	"agw/internal/assess"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/exp"
	"agw/internal/llmproxy"
	"agw/internal/notebook"
	"agw/internal/oidc"
	"agw/internal/platform"
	"agw/internal/pool"
	"agw/internal/sandbox"
	"agw/internal/store"
	"agw/internal/titler"
	"agw/internal/toolset"
	"agw/internal/watch"
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
	// The bindings of every new chat (issue #29); a typo stops the start.
	toolsets, err := env.Toolset()
	if err != nil {
		return err
	}
	for _, k := range config.LegacyPoolSizeVars {
		if os.Getenv(k) != "" {
			slog.Warn("variable ignored since AGW_TOOLSETS; use AGW_POOL_SIZE", "variable", k)
		}
	}
	// X3: the defaults of the experiment run limits are placeholders until the pilot runs (XE5) fix them.
	if env.ExpPlaceholderLimits {
		slog.Warn("experiment limits are placeholders; set them after the pilot runs",
			"max_cost_usd", env.ExpMaxCostUSD, "max_tokens", env.ExpMaxTokens,
			"max_duration", env.ExpMaxDuration.String(), "temperature", env.ExpTemperature)
	}
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
	var expAcct *oidc.PasswordSource
	var expAccount func(context.Context) (oidc.User, error)
	if env.AuthMode == config.AuthOIDC {
		if auth, err = oidc.New(oidc.Config{Issuer: env.OIDCIssuer, ClientID: env.OIDCClientID, ClientSecret: env.OIDCClientSecret, PublicURL: env.PublicURL}); err != nil {
			return err
		}
		// X1.2: the experiment account logs in with a password grant, so its runs need no browser
		// session and no 12-hour limit. The password stays in the secret file and in this process.
		switch {
		case env.ExpUser == "" && env.ExpPassword == "":
		case env.ExpUser == "" || env.ExpPassword == "":
			return errors.New("AGW_EXP_USER and AGW_EXP_PASSWORD belong together")
		case env.OIDCIssuer == "":
			return errors.New("AGW_EXP_USER needs AGW_OIDC_ISSUER (AGW_AUTH_MODE=oidc)")
		default:
			expAcct = oidc.NewPasswordSource(env.OIDCIssuer, env.ExpClientID, expClientSecret(env), env.ExpUser, env.ExpPassword)
			slog.Info("experiment account on", "user", env.ExpUser, "client", env.ExpClientID)
			if env.PlatformExchange && env.ExpClientID != env.PlatformClientID {
				// Keycloak only exchanges a token of the requesting client (or one whose audience
				// names it); a token of another client is refused with "Client is not within the
				// token audience".
				slog.Warn("experiment account logs in through a client other than the exchanging one",
					"experiment_client", env.ExpClientID, "exchange_client", env.PlatformClientID)
			}
		}
		if env.ExpAPIToken != "" {
			if len(env.ExpAPIToken) < 32 {
				return errors.New("AGW_EXP_API_TOKEN must have at least 32 characters")
			}
			if expAcct == nil {
				return errors.New("AGW_EXP_API_TOKEN needs AGW_EXP_USER and AGW_EXP_PASSWORD")
			}
		}
		expAccount = expAccountFunc(expAcct, env.ExpUser)
		// The UI is opened under the public address; its host is therefore allowed.
		if h := env.PublicHost(); h != "" && !slices.Contains(env.AllowedHosts, h) {
			env.AllowedHosts = append(env.AllowedHosts, h)
		}
		slog.Info("login through the platform (OIDC)", "issuer", env.OIDCIssuer, "client", env.OIDCClientID, "redirect", env.PublicURL+oidc.CallbackPath, "base", env.BasePath+"/", "embedding", env.FrameAncestors)
	}
	// Without oidc mode the fixture routes of #129 still work with AGW_EXP_API_TOKEN alone; the
	// runner's chat routes need the account (sub) and stay off.
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
		Exchange: env.PlatformExchange, Audiences: env.PlatformAudiences,
		ChatTokenMaxAge: env.PlatformTokenMaxAge, CallKey: store.ToolCallFrom,
		// One dataset archive of a notebook run is streamed, not held (own code C-E3).
		MaxStream: int64(env.NotebookDatasetMaxMB) << 20}
	konto := env.PlatformUser
	if auth != nil {
		// Every chat acts for its owner: the owner's token from the session is the subject_token; an
		// experiment chat uses the password grant of the experiment account (X1.2).
		pcfg.Subject = ownerToken(st, auth, expAcct)
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
	// X4 (issue #129): the fixture service works with the tokens of the two experiment accounts, never
	// through a chat. It is off without a platform binding or without credentials; a fixture of an
	// owner whose account is missing then fails with a message naming the variable.
	var fixtures *exp.Fixtures
	if env.PlatformAPIURL != "" && (env.ExpUser != "" || env.Exp2User != "") {
		primary, err := fixtureClient(env, env.ExpUser, env.ExpPassword)
		if err != nil {
			return fmt.Errorf("experiment account %s: %w", env.ExpUser, err)
		}
		secondary, err := fixtureClient(env, env.Exp2User, env.Exp2Password)
		if err != nil {
			return fmt.Errorf("experiment account %s: %w", env.Exp2User, err)
		}
		fixtures = &exp.Fixtures{Store: st, Primary: primary, Secondary: secondary}
		slog.Info("experiment fixtures on", "primary", env.ExpUser != "", "secondary", env.Exp2User != "",
			"api_key", env.ExpAPIToken != "")
	} else {
		slog.Info("experiment fixtures off (AGW_EXP_USER/AGW_EXP2_USER or AGW_PLATFORM_API_URL empty)")
	}
	// C-E3 (own code in the agent's notebook server): without AGW_JUPYTERHUB_API_URL the service is
	// not built, the manager registers no doer for its paths, and every notebook tool answers "not
	// configured on this instance" (G1.5, G1.13).
	var nb *notebook.Notebooks
	var notebooker chat.Notebooker
	if env.JupyterHubAPIURL != "" {
		nb = notebook.New(notebook.Config{
			APIURL: env.JupyterHubAPIURL, Token: env.JupyterHubToken, PublicURL: env.JupyterHubPublicURL,
			Prefix: env.NotebookPrefix, TimeoutMaxMin: env.NotebookTimeoutMaxMin,
			DatasetMaxMB: env.NotebookDatasetMaxMB, IdleMin: env.NotebookIdleMin,
		}, plat, st, rt)
		notebooker = nb
		slog.Info("notebooks on", "hub", env.JupyterHubAPIURL, "server_prefix", env.NotebookPrefix,
			"timeout_max_minutes", env.NotebookTimeoutMaxMin, "idle_minutes", env.NotebookIdleMin)
	}
	// Warm slots only for the configured combination and the experiment variants of
	// AGW_EXPERIMENT_POOL (X0.3); chats stored with another one (cli, mcp, api, both, exp:…) get a
	// slot on demand when they resume.
	targets := map[string]int{toolsets.Key(): env.PoolSize}
	for variant, n := range env.ExperimentPool {
		targets[variant] = n
	}
	p := pool.New[chat.Agent](fac.Create, fac.Destroy, targets)
	p.SetKnown(toolset.ValidVariant)
	// R0 — result checks: the gateway runs a model in a sealed container of its own and reports the
	// figures and the verdict (internal/assess). Without a container runtime or without AGW_IMAGE the
	// service cannot check anything: the capability "checks" says so and every check tool answers
	// "not configured on this instance" (G1.13).
	checks := assess.New(assess.NewRunner(rt, env.Image), st, env.AssessMaxPerChat)
	// R3 — the deployment check: the gateway may send test requests to the containers of the simulated
	// edge devices of the operator's list (AGW_EDGE_SIM_DEVICES) and to no other container. Without the
	// list the tool answers that this setup cannot probe a deployment (G1.13).
	checks.Devices = rt
	checks.EdgeDevices = env.EdgeSimDevices
	if checks.Configured() {
		slog.Info("result checks on", "image", env.Image, "max_per_chat", env.AssessMaxPerChat,
			"download_max_mb", env.AssessDownloadMaxMB, "vision_dataset_pattern", env.VisionDatasetPattern)
	} else {
		slog.Info("result checks off (no container runtime or AGW_IMAGE)")
	}
	if n := len(env.EdgeSimDevices); n > 0 {
		slog.Info("deployment checks on simulated edge devices", "devices", n)
	} else {
		slog.Info("deployment checks off (AGW_EDGE_SIM_DEVICES empty)")
	}
	m := chat.NewManager(st, p, cat, blobs, artifacts.NewBroker(), chat.Options{
		IdleTimeout: env.IdleTimeout, ApprovalTimeout: env.ApprovalTimeout,
		ArtifactMaxBytes: env.ArtifactMaxBytes, InternetDefault: env.InternetDefault, ImageMaxBytes: env.ImageMaxBytes,
		WorkspaceMaxBytes: env.WorkspaceMaxBytes,
		MaxSubagents:      env.MaxSubagents,
		ComputeMaxPerChat: env.ComputeMaxPerChat,
		PlanMaxDuration:   env.PlanMaxDuration, // A-E1: budget limits of autonomous plans
		PlanMaxLLMCost:    env.PlanMaxLLMCost,
		// X3: limits of an experiment run at the model pass-through.
		ExpMaxCostUSD:      env.ExpMaxCostUSD,
		ExpMaxTokens:       env.ExpMaxTokens,
		ExpMaxDuration:     env.ExpMaxDuration,
		ExpTemperature:     env.ExpTemperature,
		TrashRetentionDays: env.TrashRetentionDays,
		AutoCompactDefault: env.AutoCompactDefault, CompactReserveTokens: env.CompactReserveTokens, CompactKeepRecent: env.CompactKeepRecent,
		BgWakesPerHour: env.BgWakesPerHour, BgKeepAlive: env.BgKeepAlive, AutoTurnsMax: env.AutoTurnsMax,
		Titler: newTitler(cat, env.TitleModel), Platform: plat, Toolsets: toolsets,
		ExperimentUsers: env.ExperimentUsers, // X0.2: who may create experiment chats
		Sessions:        sessionKeeper(auth), // A-E3: keeps the login of plan owners fresh
		Notebook:        notebooker,
		Checks:          checks, // R0: answers the capability "checks"
	})
	fac.Backend = m
	plat.SetOnExchange(m.PlatformExchanged)

	// V1 — pre-annotation with CVAT: the calls of the annotation_ tools are answered by this service
	// instead of the platform (E4). Without both URLs the path stays unregistered and answers
	// "not configured" (G1.13).
	if env.CVATAPIURL != "" && plat != nil {
		cvat, err := annotation.NewCVAT(env.CVATAPIURL, nil)
		if err != nil {
			return err
		}
		svc := annotation.New(plat, st, cvat, env.AnnotationFunctions, env.CVATPublicURL,
			env.AnnotationMaxFunctions, env.AnnotationModelMaxMB)
		// V3d: the image review runs with the chat's model, and only where #111 allows it — a self hosted
		// model may see every dataset, a cloud model only those whose name matches the pattern. Every call
		// it makes is an auxiliary call of the chat, like a title (never an llm_call of the agent).
		pattern, err := assess.VisionPattern(env.VisionDatasetPattern)
		if err != nil {
			return fmt.Errorf("AGW_VISION_DATASET_PATTERN: %w", err)
		}
		svc.Cat, svc.VisionPattern = cat, pattern
		svc.Aux = func(ctx context.Context, call store.AuxCall) {
			if err := st.RecordAuxCall(ctx, call); err != nil {
				slog.Warn("image review call not recorded", "chat", call.ChatID, "error", err)
			}
		}
		m.RegisterVirtual(platform.AnnotationPrefix, svc)
		svc.RegisterWatch()
		slog.Info("pre-annotation on", "cvat", env.CVATAPIURL, "functions", len(env.AnnotationFunctions),
			"max_functions", env.AnnotationMaxFunctions, "model_max_mb", env.AnnotationModelMaxMB,
			"vision_dataset_pattern", env.VisionDatasetPattern)
	} else {
		slog.Info("pre-annotation off (AGW_CVAT_API_URL or the platform binding is empty)")
	}

	// V3 — the checks and the fills of pre-annotation are background jobs of the gateway (one at a
	// time, minutes of CPU time). Their observer reads only the row in annotation_jobs and never the
	// platform: the closure below gets the store, and the kind is registered with its route here
	// (G1.7) next to the kinds of V1 (which the service registers itself).
	watch.Register(annotation.JobWatch(st))
	watch.RegisterRoute(http.MethodPost, annotation.JobRoutePath(), annotation.JobKindName, annotation.JobRef)

	if nb != nil {
		// The notebook paths are answered by the gateway itself (G1.5); classification, approval,
		// compute limit and log run unchanged in PlatformCall. The dataset archives the runner
		// streams are logged here (the agent's own calls are logged by the socket).
		m.RegisterVirtual(platform.NotebookPrefix, nb)
		nb.LogCall = func(ctx context.Context, chatID, detail, result string) {
			m.LogCallBy(ctx, m.SlotOf(chatID), chatID, "platform", "platform", detail, result)
		}
		method, path := notebook.RunRoute()
		watch.RegisterRoute(method, path, notebook.KindName, notebook.RunIDFrom)
		watch.Register(nb.WatchKind())
	}

	// R0 — the check paths are answered by the gateway itself (G1.5): delegation, effect class and log
	// run as for every other call, and the gateway decides what a check runs on. Checks are observed like
	// runs, so the job line of the watcher shows their state (R0.21); one check runs at a time (R0.20).
	m.RegisterVirtual(platform.AssessPrefix, checks)
	{
		method, path := assess.CheckRoute()
		watch.RegisterRoute(method, path, assess.KindName, assess.RunIDFrom)
		watch.Register(checks.WatchKind())
	}

	// R1 — the training check: the gateway splits a dataset before a training and measures the model
	// afterwards on the images the training did not see. It does both with the user's token, and it is
	// the only place that records where a model came from (R1.6): without such a row every check of that
	// model answers "origin unknown" (P2).
	if plat != nil {
		checks.Plat = plat
		checks.MaxDownloadBytes = int64(env.AssessDownloadMaxMB) << 20
		// The downloads and the created training dataset appear in the activity like the dataset
		// archives of a notebook run (C-E3).
		checks.LogCall = func(ctx context.Context, chatID, detail, result string) {
			m.LogCallBy(ctx, m.SlotOf(chatID), chatID, "platform", "platform", detail, result)
		}
		platform.SetOnModelCreated(func(ctx context.Context, chatID string, modelID, trainID, datasetID int64) {
			seen := []int64{}
			if datasetID > 0 {
				seen = append(seen, datasetID)
			}
			if err := st.RecordModelOrigin(ctx, store.ModelOrigin{
				ModelID: modelID, ChatID: chatID, Source: store.OriginTraining,
				TrainContainerID: trainID, SeenDatasetIDs: seen,
			}); err != nil {
				slog.Error("model origin not recorded", "chat", chatID, "model", modelID, "error", err)
			}
		})
		// R2 — a model uploaded from a file of the sandbox keeps its origin when the gateway had handed
		// that file out of a notebook run of this chat (R2.6): so the check of the file stays the check of
		// the model, and the approval of an image build names either of them. A file the agent brought
		// along itself records nothing (P2); assess.RecordUploadedModel decides that.
		platform.SetOnModelUploaded(func(ctx context.Context, chatID string, modelID int64, sha256 string) {
			if err := assess.RecordUploadedModel(ctx, st, chatID, modelID, sha256); err != nil {
				slog.Error("model origin of an upload not recorded", "chat", chatID, "model", modelID, "error", err)
			}
		})
	}

	if err := m.Recover(ctx); err != nil {
		return err
	}
	poolCtx, poolCancel := context.WithCancel(context.Background())
	p.Start(poolCtx)
	// Chats in the trash past AGW_TRASH_RETENTION_DAYS are deleted for good (off by default; issue #76).
	go m.RunTrashRetention(poolCtx)
	// A-E3: the endurance loop of autonomous plans. It renews the login of plan owners ahead of time and
	// continues their plans when a login comes back. Without a login through the platform (AGW_AUTH_MODE
	// != oidc) there is no session to keep, and the loop says so and returns.
	go m.RunPlanEndurance(poolCtx)
	// A-E2: the watcher of observed platform jobs. It polls the platform with the user's token until a
	// job ends, writes its progress into watch_jobs, distributes it and wakes the agent when a plan step
	// is through. Without a platform binding (AGW_PLATFORM_API_URL empty) it says so and returns. The
	// kinds themselves are registered with internal/watch next to the feature that owns them (see
	// watch.Register for the notebook and the pre-annotation above).
	go m.RunWatchers(poolCtx)
	// Notebook servers of chats that had no run for a while are stopped (own code C-E3); the volumes
	// with the files stay.
	if nb != nil {
		go nb.RunReaper(poolCtx)
	}

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
	// A typed nil would satisfy the interface and turn the 503 of an unconfigured instance into a
	// panic on the first call, so only a built service is handed over.
	var expFixtures api.ExpFixtures
	if fixtures != nil {
		expFixtures = fixtures
	}
	apiSrv := &http.Server{Addr: env.HTTPAddr, Handler: (&api.Server{M: m, Pool: p, Cat: cat, Env: env, Web: webFS, Blocked: blocked, Token: env.APIToken, AllowedHosts: env.AllowedHosts,
		OIDC: auth, FrameAncestors: env.FrameAncestors, Platform: plat, ExpFixtures: expFixtures,
		ExpAPIToken: env.ExpAPIToken, ExpAccount: expAccount}).Handler(),
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
	slog.Info("orchestrator running", "api", env.HTTPAddr, "proxy", env.ProxyAddr, "toolsets", toolsets.Key(), "pool", env.PoolSize, "model", cat.Default, "image", env.Image)

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

// sessionKeeper hands the OIDC service to the manager as the login side of A-E3. In the token mode there is
// no service, and nil is passed on: a nil *oidc.Service in the interface would not be nil, and the endurance
// loop would then keep asking an empty service for sessions.
func sessionKeeper(auth *oidc.Service) chat.SessionKeeper {
	if auth == nil {
		return nil
	}
	return auth
}

// fixtureClient is the password grant of one experiment account for the fixtures (X4). It uses the
// same public client as the experiment account of X1.2 (AGW_EXP_CLIENT_ID, default frontend):
// agw-agent is confidential, and Keycloak rejects a password grant without its client secret
// (unauthorized_client).
func fixtureClient(env config.Env, user, password string) (*platform.Client, error) {
	if user == "" || password == "" {
		return nil, nil
	}
	return platform.New(platform.Config{
		APIURL: env.PlatformAPIURL, TokenURL: env.PlatformTokenURL,
		ClientID: env.ExpClientID, ClientSecret: expClientSecret(env), User: user, Password: password,
	})
}

// expClientSecret is the secret of the experiment account's login client. It is only used when the
// configured client is the gateway's confidential client; the public "frontend" needs none.
func expClientSecret(env config.Env) string {
	if env.ExpClientID != "" && env.ExpClientID == env.PlatformClientID {
		return env.PlatformClientSecret
	}
	return ""
}

// ownerToken returns, per chat, the access token of its owner from the owner's session at the orchestrator.
// Without a live session the platform call fails with oidc.ErrNoSession (message to the agent).
func ownerToken(st *store.Store, auth *oidc.Service, exp *oidc.PasswordSource) func(ctx context.Context, chatID string) (string, error) {
	return func(ctx context.Context, chatID string) (string, error) {
		owner, err := st.ChatOwner(ctx, chatID)
		if err != nil {
			return "", err
		}
		if owner == "" {
			return "", errors.New("chat without owner (created in token mode); create a new chat for the platform")
		}
		// X1.2: chats of the experiment account carry its sub; its token comes from the password
		// grant, not from a browser session.
		if exp != nil && exp.Owns(ctx, owner) {
			return exp.Token(ctx)
		}
		return auth.AccessToken(ctx, owner)
	}
}

// expAccountFunc resolves the experiment account for the API key (X1.3): the sub becomes the owner
// of the chats it creates, the user name is what AGW_EXPERIMENT_USERS checks.
func expAccountFunc(exp *oidc.PasswordSource, user string) func(context.Context) (oidc.User, error) {
	if exp == nil {
		return nil
	}
	return func(ctx context.Context) (oidc.User, error) {
		sub, err := exp.Subject(ctx)
		if err != nil {
			return oidc.User{}, err
		}
		return oidc.User{Sub: sub, Username: user}, nil
	}
}

// newTitler: chat titles from the chat's model or from AGW_TITLE_MODEL; "off" switches them off.
func newTitler(cat *config.Catalog, model string) chat.Titler {
	if model == "off" {
		return nil
	}
	return titler.New(cat, model)
}
