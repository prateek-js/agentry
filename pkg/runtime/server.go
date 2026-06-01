// Package runtime provides the HTTP server for the sandbox runtime.
package runtime

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentry/agentry/pkg/auth"
	"github.com/agentry/agentry/pkg/handlers"
	"github.com/agentry/agentry/pkg/jupyter"
	"github.com/agentry/agentry/pkg/shell"
	"github.com/agentry/agentry/pkg/telemetry"
)

// APIKeyEnv is the environment variable used to enable API-key auth.
// When unset or empty, the runtime accepts unauthenticated requests.
const APIKeyEnv = "SANDBOX_API_KEY"

// Server is the sandbox runtime HTTP server.
type Server struct {
	httpServer *http.Server
	shellMgr   *shell.Manager
	bgMgr      *shell.BackgroundManager
	ptyMgr     *shell.PTYManager
	jupyterMgr *jupyter.Manager
	projectMgr *handlers.ProjectManager
	auth       *auth.Authenticator
}

// New creates a new runtime server. Reads $SANDBOX_API_KEY for optional auth.
func New(addr string) *Server {
	return NewWithKey(addr, os.Getenv(APIKeyEnv))
}

// NewWithKey is like New but takes the API key explicitly (useful for tests).
// An empty key disables auth.
func NewWithKey(addr, apiKey string) *Server {
	shellMgr := shell.NewManager()
	bgMgr := shell.NewBackgroundManager()
	ptyMgr := shell.NewPTYManager()
	jupyterMgr := jupyter.NewManager()
	projectMgr := handlers.NewProjectManager("")
	authn := auth.New(apiKey, "/health")

	mux := http.NewServeMux()
	registerRoutes(mux, shellMgr, bgMgr, ptyMgr, jupyterMgr, projectMgr)

	// Chain: CONNECT (outermost, so the upgrade hijack happens before
	// any middleware caches the ResponseWriter) → CORS → telemetry →
	// auth → routes.
	//
	// CONNECT is the data-plane verb. Its target ("127.0.0.1:<port>")
	// doesn't match any HTTP route; the connectInterceptor diverts
	// it to handlers.ForwardConnectHandler, which dials the named
	// local port and pipes bytes. Everything else flows down into
	// the normal HTTP route table for the control plane.
	httpHandler := corsMiddleware(telemetry.HTTPMiddleware(authn.Middleware(mux)))
	handler := connectInterceptor(httpHandler)

	return &Server{
		httpServer: &http.Server{
			Addr:         addr,
			Handler:      handler,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 600 * time.Second, // Long for shell commands
			IdleTimeout:  120 * time.Second,
		},
		shellMgr:   shellMgr,
		bgMgr:      bgMgr,
		ptyMgr:     ptyMgr,
		jupyterMgr: jupyterMgr,
		projectMgr: projectMgr,
		auth:       authn,
	}
}

// Handler returns the configured HTTP handler chain (CORS → auth → routes).
// Exposed for tests so they can drive the server via httptest.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Run starts the server and blocks until shutdown signal.
func (s *Server) Run() error {
	auth.LogStartup("runtime", APIKeyEnv, s.auth)

	// Graceful shutdown on SIGTERM/SIGINT.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Printf("ad-sandbox runtime listening on %s", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Auto-start projects.
	go s.projectMgr.AutoStartProjects()

	// Wait for shutdown signal.
	sig := <-stop
	log.Printf("received %v, shutting down...", sig)

	// Stop all projects.
	s.projectMgr.StopAll()
	s.shellMgr.CloseAll()
	s.bgMgr.Shutdown()
	s.ptyMgr.CloseAll()
	s.jupyterMgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// connectInterceptor diverts CONNECT requests to the byte-pipe data
// plane and passes everything else through to the HTTP control
// plane. CONNECT is identified by HTTP method, not by URL — its
// request target is "host:port" rather than a path, and the upgrade
// must hijack before any middleware caches the writer.
func connectInterceptor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handlers.ForwardConnectHandler(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func registerRoutes(mux *http.ServeMux, shellMgr *shell.Manager, bgMgr *shell.BackgroundManager, ptyMgr *shell.PTYManager, jupyterMgr *jupyter.Manager, projectMgr *handlers.ProjectManager) {
	// Health & info
	mux.HandleFunc("GET /health", handlers.HealthHandler)
	mux.HandleFunc("GET /v1/sandbox", handlers.SandboxHandler)

	// Shell
	mux.HandleFunc("POST /v1/shell/exec", handlers.ShellHandler(shellMgr))

	// Background shell — long-running commands tracked by id with
	// bounded ring-buffer logs and process-group SIGTERM/SIGKILL.
	mux.HandleFunc("POST /v1/shell/background", handlers.BgStartHandler(bgMgr))
	mux.HandleFunc("GET /v1/shell/background", handlers.BgListHandler(bgMgr))
	mux.HandleFunc("GET /v1/shell/background/{id}", handlers.BgStatusHandler(bgMgr))
	mux.HandleFunc("GET /v1/shell/background/{id}/logs", handlers.BgLogsHandler(bgMgr))
	mux.HandleFunc("POST /v1/shell/background/{id}/interrupt", handlers.BgInterruptHandler(bgMgr))
	mux.HandleFunc("DELETE /v1/shell/background/{id}", handlers.BgForgetHandler(bgMgr))

	// Interactive PTY over WebSocket.
	mux.HandleFunc("GET /v1/shell/pty", handlers.PTYWebSocketHandler(ptyMgr))
	mux.HandleFunc("GET /v1/shell/ptys", handlers.PTYListHandler(ptyMgr))
	mux.HandleFunc("DELETE /v1/shell/pty/{id}", handlers.PTYCloseHandler(ptyMgr))

	// Code interpreter (Jupyter ZMQ kernels, multi-language).
	mux.HandleFunc("POST /v1/code/contexts", handlers.CodeCreateContextHandler(jupyterMgr))
	mux.HandleFunc("GET /v1/code/contexts", handlers.CodeListContextsHandler(jupyterMgr))
	mux.HandleFunc("DELETE /v1/code/contexts/{id}", handlers.CodeDeleteContextHandler(jupyterMgr))
	mux.HandleFunc("POST /v1/code/contexts/{id}/exec", handlers.CodeExecuteHandler(jupyterMgr))
	mux.HandleFunc("POST /v1/code/contexts/{id}/interrupt", handlers.CodeInterruptHandler(jupyterMgr))

	// File operations
	mux.HandleFunc("POST /v1/file/read", handlers.FileReadHandler)
	mux.HandleFunc("POST /v1/file/write", handlers.FileWriteHandler)
	mux.HandleFunc("POST /v1/file/list", handlers.FileListHandler)
	mux.HandleFunc("POST /v1/file/find", handlers.FileFindHandler)
	mux.HandleFunc("POST /v1/file/search", handlers.FileSearchHandler)
	mux.HandleFunc("POST /v1/file/replace", handlers.FileReplaceHandler)
	// Streaming endpoints: Range-aware download + multipart upload.
	mux.HandleFunc("GET /v1/file/download", handlers.FileDownloadHandler)
	mux.HandleFunc("POST /v1/file/upload", handlers.FileUploadHandler)

	// Process management
	mux.HandleFunc("GET /v1/process/list", handlers.ProcessListHandler)
	mux.HandleFunc("POST /v1/process/stop", handlers.ProcessStopHandler)

	// Ports
	mux.HandleFunc("GET /v1/ports", handlers.PortsListHandler)
	mux.HandleFunc("POST /v1/ports/wait", handlers.PortWaitHandler)

	// User-app reverse proxy. Browser traffic from a *.agentry.live
	// deployment URL lands here as /v1/proxy/<port>/<rest> after the
	// bridge → cluster tunnel → provisioner runtime_proxy chain.
	// Streaming-by-default so SSE + WebSocket upgrades work.
	mux.HandleFunc("/v1/proxy/{port}/{rest...}", handlers.AppProxyHandler)
	mux.HandleFunc("/v1/proxy/{port}", handlers.AppProxyHandler)

	// Port forwarding is handled OUTSIDE the mux as the CONNECT verb
	// (see connectInterceptor) — anything that speaks TCP can ride it.

	// Project manager
	mux.HandleFunc("POST /v1/project/start", handlers.ProjectStartHandler(projectMgr))
	mux.HandleFunc("POST /v1/project/stop", handlers.ProjectStopHandler(projectMgr))
	mux.HandleFunc("POST /v1/project/restart", handlers.ProjectRestartHandler(projectMgr))
	mux.HandleFunc("POST /v1/project/start-all", handlers.ProjectStartAllHandler(projectMgr))
	mux.HandleFunc("POST /v1/project/stop-all", handlers.ProjectStopAllHandler(projectMgr))
	mux.HandleFunc("GET /v1/project/list", handlers.ProjectListHandler(projectMgr))
	mux.HandleFunc("GET /v1/project/logs", handlers.ProjectLogsHandler(projectMgr))
	mux.HandleFunc("GET /v1/project/logs/stream", handlers.ProjectLogStreamHandler(projectMgr))

	// Workspace & activity
	mux.HandleFunc("GET /v1/workspace/status", handlers.WorkspaceStatusHandler(projectMgr))
	mux.HandleFunc("GET /v1/activity", handlers.ActivityHandler)

	// Metrics
	mux.HandleFunc("GET /v1/metrics", handlers.MetricsHandler)

	// Git
	mux.HandleFunc("POST /v1/git/init", handlers.GitInitHandler)
	mux.HandleFunc("POST /v1/git/commit", handlers.GitCommitHandler)
	mux.HandleFunc("POST /v1/git/diff", handlers.GitDiffHandler)
	mux.HandleFunc("POST /v1/git/status", handlers.GitStatusHandler)
	mux.HandleFunc("POST /v1/git/log", handlers.GitLogHandler)
	mux.HandleFunc("POST /v1/git/clone", handlers.GitCloneHandler)
	mux.HandleFunc("POST /v1/git/checkout", handlers.GitCheckoutHandler)

	// Archive
	mux.HandleFunc("POST /v1/archive/create", handlers.ArchiveCreateHandler)
	mux.HandleFunc("POST /v1/archive/extract", handlers.ArchiveExtractHandler)

	// Catch-all info
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		handlers.JSON(w, http.StatusOK, map[string]string{
			"name":    "ad-sandbox",
			"version": "1.0.0",
			"docs":    "https://github.com/agentry/agentry",
		})
	})

	fmt.Println("Registered routes:")
	fmt.Println("  GET  /health")
	fmt.Println("  GET  /v1/sandbox")
	fmt.Println("  POST /v1/shell/exec")
	fmt.Println("  POST /v1/file/{read,write,list,find,search,replace}")
	fmt.Println("  GET  /v1/process/list")
	fmt.Println("  POST /v1/process/stop")
	fmt.Println("  GET  /v1/ports")
	fmt.Println("  POST /v1/ports/wait")
	fmt.Println("  POST /v1/project/{start,stop,restart,start-all,stop-all}")
	fmt.Println("  GET  /v1/project/{list,logs,logs/stream}")
	fmt.Println("  GET  /v1/workspace/status")
	fmt.Println("  GET  /v1/activity")
	fmt.Println("  GET  /v1/metrics")
	fmt.Println("  POST /v1/git/{init,commit,diff,status,log,clone,checkout}")
	fmt.Println("  POST /v1/archive/{create,extract}")
}
