package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
	"github.com/mrkizildag/pollux-agent/backend/internal/config"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/github"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

const (
	maxParallelJobs        = 8
	actionsScaffoldTimeout = 10 * time.Minute
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	store, err := sqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("open database %s: %w", cfg.DatabasePath, err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Error("close database", "err", err)
		}
	}()

	ghHTTPClient := &http.Client{Timeout: 20 * time.Second}
	ghClient, err := github.NewClient(ghHTTPClient, cfg.GitHubAppID, []byte(cfg.GitHubPrivateKey.Reveal()), "")
	if err != nil {
		return fmt.Errorf("create GitHub client: %w", err)
	}
	runners, err := buildRunners(cfg, ghClient, logger)
	if err != nil {
		return fmt.Errorf("build analysis runners: %w", err)
	}
	gateSvc := gate.NewService(ghClient, ghClient, store, runners, ghClient, httpapi.NewScaffoldQueue(store)).WithLogger(logger)

	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gateSvc), logger, maxParallelJobs)
	workerCtx, cancelWorker := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWorker()

	workerErr := make(chan error, 1)
	go func() {
		workerErr <- worker.Run(workerCtx)
	}()

	sweepCtx, cancelSweep := context.WithCancel(context.WithoutCancel(ctx))
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		sweepDeadlines(sweepCtx, store, worker, logger)
	}()
	defer func() {
		cancelSweep()
		<-sweepDone
	}()

	deps := httpapi.Deps{
		Logger:        logger,
		WebhookSecret: []byte(cfg.WebhookSecret.Reveal()),
		Jobs:          worker,
		Runs:          store,
		Auth:          buildAuth(cfg.Dashboard, store, ghHTTPClient, logger),
	}
	if cfg.Dashboard != nil {
		deps.PublicOrigin = cfg.Dashboard.Origin()
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewHandler(deps),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1) // buffered so the goroutine can exit if run already returned
	go func() {
		logger.Info("listening", "addr", cfg.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		cancelWorker()
		<-workerErr
		return fmt.Errorf("serve %s: %w", cfg.Addr, err)
	case err := <-workerErr:
		shutdownErr := shutdownServer(ctx, srv)
		return errors.Join(fmt.Errorf("run worker: %w", err), shutdownErr)
	case <-ctx.Done():
	}

	if err := shutdownServer(ctx, srv); err != nil {
		cancelWorker()
		<-workerErr
		return err
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		cancelWorker()
		<-workerErr
		return fmt.Errorf("serve %s: %w", cfg.Addr, err)
	}

	cancelWorker()
	if err := <-workerErr; err != nil {
		return fmt.Errorf("run worker: %w", err)
	}
	return nil
}

// sweepDeadlines enqueues deadline jobs for overdue runs until ctx is done.
func sweepDeadlines(ctx context.Context, src httpapi.OverdueSource, jobs httpapi.Enqueuer, logger *slog.Logger) {
	ticker := time.NewTicker(httpapi.DeadlineSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := httpapi.EnqueueDeadlineJobs(ctx, src, jobs, logger, now); err != nil {
				logger.Error("sweep deadlines", "err", err)
			}
		}
	}
}

// buildAuth wires Sign in with GitHub from the dashboard config. A nil dashboard
// returns nil, which leaves the dashboard routes unregistered.
func buildAuth(dashboard *config.Dashboard, store auth.Store, httpClient *http.Client, logger *slog.Logger) *auth.Service {
	if dashboard == nil {
		return nil
	}
	user := github.NewUserClient(httpClient, dashboard.ClientID, dashboard.ClientSecret.Reveal(), "", "")
	opts := auth.Options{
		ClientID:     dashboard.ClientID,
		AuthorizeURL: user.AuthorizeURL(),
		RedirectURL:  dashboard.PublicURL.JoinPath("auth", "callback").String(),
		Logger:       logger,
	}
	copy(opts.Key[:], dashboard.SessionKey.Reveal())
	return auth.NewService(store, user, opts)
}

// llmHTTPTimeout is longer than the GitHub client's 20s: chat completions
// take longer than a REST call.
const llmHTTPTimeout = 60 * time.Second

// buildRunners wires the Actions runner and, from cfg.LLM, the server runner. A nil cfg.LLM
// leaves the server slot empty, so a repo must run the Actions workflow.
func buildRunners(cfg config.Config, ghClient *github.Client, logger *slog.Logger) (gate.Runners, error) {
	actionsRunner := actions.New(ghClient, gate.AnalysisDeadline, actionsScaffoldTimeout)
	if cfg.LLM == nil {
		return gate.Runners{Actions: actionsRunner}, nil
	}

	model, err := llm.New(string(cfg.LLM.Provider), &http.Client{Timeout: llmHTTPTimeout}, cfg.LLM.BaseURL, cfg.LLM.APIKey.Reveal())
	if err != nil {
		return gate.Runners{}, fmt.Errorf("build LLM model: %w", err)
	}

	backend := llmrunner.NewBackend(model, ghClient.InstallationToken, cfg.LLM.Model, logger)
	runner := pipeline.NewSync(backend, llmrunner.NewJudge(model, cfg.LLM.TriageModel), logger)
	return gate.Runners{Actions: actionsRunner, Server: runner}, nil
}

func shutdownServer(ctx context.Context, srv *http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
