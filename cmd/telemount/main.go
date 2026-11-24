package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"

	"telemount/internal/db"
	"telemount/internal/state"
	"telemount/internal/storage"
	"telemount/internal/tg"
	"telemount/internal/web"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	headless := flag.Bool("headless", false, "run without desktop system tray or virtual drive mount")
	minimized := flag.Bool("minimized", false, "start minimized in system tray without opening browser")
	shellAction := flag.String("shell-action", "", "internal Explorer context menu action (pin|free|sync|status)")
	shellPath := flag.String("path", "", "target Windows path for --shell-action")
	flag.Parse()

	defDataDir, defTmpDir, defListen := defaultRuntimePaths()
	listenAddr := envOr("LISTEN_ADDR", defListen)
	dataDir := envOr("DATA_DIR", defDataDir)
	tmpDir := envOr("TMP_DIR", defTmpDir)

	if *shellAction != "" {
		runShellAction(dataDir, *shellAction, *shellPath)
		return
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("create data dir %s: %v", dataDir, err)
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		log.Fatalf("create tmp dir %s: %v", tmpDir, err)
	}

	st, err := state.Load(dataDir)
	if err != nil {
		log.Fatalf("load local state: %v", err)
	}

	store, err := db.Open(filepath.Join(dataDir, "telemount.db"))
	if err != nil {
		log.Fatalf("open sqlite db: %v", err)
	}
	defer store.Close()

	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	tgMgr := tg.NewManager(dataDir)
	defer tgMgr.Stop()

	snap := st.Snapshot()
	if snap.ChannelID != 0 {
		tgMgr.SetChannel(snap.ChannelID, snap.ChannelAccessHash)
	}
	if snap.TGAPIID > 0 && snap.TGAPIHash != "" {
		go func() {
			if err := tgMgr.Start(appCtx, snap.TGAPIID, snap.TGAPIHash); err != nil {
				log.Printf("telegram auto-connect warning: %v", err)
			}
		}()
	}

	engine := storage.NewEngine(store, st, tgMgr, tmpDir)
	defer engine.Pool().StopAll()

	for _, acc := range snap.BackupAccounts {
		acc := acc
		apiID, apiHash := acc.APIID, acc.APIHash
		if apiID <= 0 || apiHash == "" {
			apiID, apiHash = snap.TGAPIID, snap.TGAPIHash
		}
		if apiID <= 0 || apiHash == "" {
			continue
		}
		bm := engine.Pool().GetOrCreateManager(acc.ID)
		if acc.OwnChannelID != 0 {
			bm.SetChannel(acc.OwnChannelID, acc.OwnChannelAccessHash)
		}
		go func() {
			if err := bm.Start(appCtx, apiID, apiHash); err != nil {
				log.Printf("backup telegram (%s) auto-connect warning: %v", acc.ID, err)
			}
		}()
	}

	engine.StartBackgroundTasks(appCtx)
	srv := web.NewServer(engine, tgMgr)

	runPlatform(appCtx, appCancel, dataDir, listenAddr, *headless, *minimized, store, st, tgMgr, engine, srv)
}
