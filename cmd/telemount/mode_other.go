//go:build !windows

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"telemount/internal/db"
	"telemount/internal/state"
	"telemount/internal/storage"
	"telemount/internal/tg"
	"telemount/internal/web"
)

func defaultRuntimePaths() (dataDir, tmpDir, listenAddr string) {
	return "/data", "/tmp/telemount", ":8080"
}

func runShellAction(_, _, _ string) {
	log.Fatal("--shell-action is only supported on Windows desktop")
}

func runPlatform(
	_ context.Context,
	appCancel context.CancelFunc,
	dataDir, listenAddr string,
	_, _ bool,
	_ *db.Store,
	_ *state.Manager,
	_ *tg.Manager,
	engine *storage.Engine,
	srv *web.Server,
) {
	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down TeleMount (flushing any buffered RAM pack)...")
		shutCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		if engine.IsReady() {
			if err := engine.Packer().FlushNow(shutCtx); err != nil {
				log.Printf("flush pack on shutdown warning: %v", err)
			}
		}
		appCancel()
	}()

	log.Printf("TeleMount server listening on %s (Web UI: /, WebDAV: /dav/, data: %s)", listenAddr, dataDir)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
}
