// ad-sandbox runtime — secure execution environment for AI agents.
//
// Usage:
//
//	sandbox-runtime                    # Listen on :8080
//	sandbox-runtime -addr :9090        # Listen on :9090
//	SANDBOX_ID=abc123 sandbox-runtime  # Set sandbox identity
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/agentry/agentry/pkg/runtime"
	"github.com/agentry/agentry/pkg/telemetry"
)

const runtimeVersion = "1.0.0"

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	if env := os.Getenv("SANDBOX_PORT"); env != "" {
		*addr = ":" + env
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Printf("ad-sandbox runtime starting (pid=%d)", os.Getpid())

	// Telemetry: no-op if OTEL_EXPORTER_OTLP_ENDPOINT is unset.
	telCtx, telCancel := context.WithTimeout(context.Background(), 10*time.Second)
	shutdown, err := telemetry.Init(telCtx, telemetry.ConfigFromEnv("agentry-runtime", runtimeVersion))
	telCancel()
	if err != nil {
		log.Printf("telemetry init failed (continuing without): %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()

	server := runtime.New(*addr)
	if err := server.Run(); err != nil {
		log.Fatalf("server shutdown error: %v", err)
	}
	log.Println("ad-sandbox runtime stopped")
}
