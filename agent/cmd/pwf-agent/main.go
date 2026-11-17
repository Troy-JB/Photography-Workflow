// Command pwf-agent is the headless half of the Windows agent: import, resume,
// status and self-test. The Wails GUI shares the same core packages.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"time"

	"photography-workflow/agent/internal/api"
	"photography-workflow/agent/internal/card"
	"photography-workflow/agent/internal/core"
	"photography-workflow/agent/internal/derive"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/remote"
	"photography-workflow/agent/internal/selftest"
	"photography-workflow/agent/internal/settings"
	"photography-workflow/agent/internal/source"
	"photography-workflow/agent/internal/store"
	"photography-workflow/agent/internal/tool"
	"photography-workflow/agent/internal/updater"
	"photography-workflow/agent/internal/upload"
)

const (
	exitOK         = 0
	exitError      = 1
	exitIncomplete = 2
	exitPaused     = 3
	exitDuplicate  = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitOK
	}
	switch args[0] {
	case "import":
		return cmdImport(args[1:])
	case "resume":
		return cmdResume(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "list":
		return cmdList(args[1:])
	case "cards":
		return cmdCards(args[1:])
	case "login":
		return cmdLogin(args[1:])
	case "upload":
		return cmdUpload(args[1:])
	case "update-check":
		return cmdUpdateCheck(args[1:])
	case "selftest":
		return cmdSelftest(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		return exitError
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `pwf-agent - Photography Workflow capture agent

Usage:
  pwf-agent import  -src DIR -project CODE -a DIR (-b DIR | -no-backup) [flags]
  pwf-agent resume  [-id IMPORT] [flags]
  pwf-agent status  -id IMPORT [-json]
  pwf-agent list    [-json]
  pwf-agent cards   [-json]
  pwf-agent login   -server URL -email EMAIL -password PW [-tenant SLUG] [flags]
  pwf-agent upload  -id IMPORT [-server URL] [-json] [flags]
  pwf-agent update-check [-manifest URL -key BASE64] [-channel dev|stable] [-json]
  pwf-agent selftest [-json]

Upload exit codes: 0 remote verified, 1 error, 2 not finished, 3 paused.
Update-check exit codes: 0 up to date, 1 error, 2 update available.
The database defaults to %LOCALAPPDATA%\PWF\agent\state.db (PWF_AGENT_DB overrides);
the server settings file defaults to settings.json next to the database.
`)
}

func defaultDBPath() string {
	if p := os.Getenv("PWF_AGENT_DB"); p != "" {
		return p
	}
	if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
		return filepath.Join(dir, "PWF", "agent", "state.db")
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "pwf-agent", "state.db")
	}
	return "pwf-agent.db"
}

func cmdImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	srcRoot := fs.String("src", "", "card root or folder to import")
	projectCode := fs.String("project", "", "project code used in the destination layout")
	projectID := fs.String("project-id", "", "server project id")
	shootDate := fs.String("date", time.Now().Format("2006-01-02"), "shoot date (YYYY-MM-DD)")
	destA := fs.String("a", "", "primary destination directory")
	destB := fs.String("b", "", "backup destination directory")
	noBackup := fs.Bool("no-backup", false, "run with a single destination (persistent warning)")
	workers := fs.Int("workers", 2, "parallel copy workers")
	allowDuplicate := fs.Bool("new", false, "start a new import even if the card has an active one")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	dbPath := fs.String("db", defaultDBPath(), "state database path")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *srcRoot == "" || *projectCode == "" || *destA == "" {
		fmt.Fprintln(os.Stderr, "import requires -src, -project and -a")
		return exitError
	}
	if !*noBackup && *destB == "" {
		fmt.Fprintln(os.Stderr, "destination B is required; pass -b DIR, or -no-backup to downgrade explicitly")
		return exitError
	}

	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fail(err)
	}
	defer st.Close()

	src, err := source.NewDir(*srcRoot)
	if err != nil {
		return fail(err)
	}
	deriver := newDeriver(st, *dbPath)
	defer deriver.Close()
	importer := core.New(st, core.Options{Workers: *workers, Enrich: deriver.Enrich})
	req := core.StartRequest{
		ProjectID:      *projectID,
		ProjectCode:    *projectCode,
		ShootDate:      *shootDate,
		Source:         src,
		DestA:          *destA,
		DestB:          *destB,
		RequireBackup:  !*noBackup,
		Card:           card.ForRoot(ctx, src.Root()),
		AllowDuplicate: *allowDuplicate,
	}
	im, added, err := importer.Start(ctx, req)
	if errors.Is(err, core.ErrImportExists) {
		if *jsonOut {
			printJSON(map[string]any{"error": "import_exists", "existing": im})
		} else {
			fmt.Printf("card already has an active import: %s (status %s)\n", im.ID, im.Status)
			fmt.Printf("resume it with: pwf-agent resume -id %s   (or pass -new to import again)\n", im.ID)
		}
		return exitDuplicate
	}
	if err != nil {
		return fail(err)
	}
	if !*jsonOut {
		fmt.Printf("import %s created: %d files\n", im.ID, added)
	}
	return runImport(ctx, st, importer, im, *jsonOut)
}

func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	id := fs.String("id", "", "import id (defaults to the latest open import)")
	workers := fs.Int("workers", 2, "parallel copy workers")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	dbPath := fs.String("db", defaultDBPath(), "state database path")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fail(err)
	}
	defer st.Close()

	var im *model.Import
	if *id != "" {
		im, err = st.Import(ctx, *id)
		if err != nil {
			return fail(err)
		}
	} else {
		imports, err := st.Imports(ctx, 100)
		if err != nil {
			return fail(err)
		}
		for i := range imports {
			if imports[i].Status == model.ImportOpen {
				im = &imports[i]
				break
			}
		}
		if im == nil {
			fmt.Fprintln(os.Stderr, "no open import to resume")
			return exitError
		}
	}
	deriver := newDeriver(st, *dbPath)
	defer deriver.Close()
	importer := core.New(st, core.Options{Workers: *workers, Enrich: deriver.Enrich})
	return runImport(ctx, st, importer, im, *jsonOut)
}

// newDeriver resolves the helper binaries the same way the GUI does: explicit
// PWF_EXIFTOOL / PWF_FFMPEG overrides, then the tools directory next to the
// executable, then PATH. Missing helpers degrade to a warning, never a failure.
func newDeriver(st *store.Store, dbPath string) *derive.Derive {
	paths := tool.Locate(os.Getenv("PWF_EXIFTOOL"), os.Getenv("PWF_FFMPEG"))
	return derive.New(st,
		filepath.Join(filepath.Dir(dbPath), "cache", "previews"),
		paths.ExifTool, paths.FFmpeg, nil)
}

func runImport(ctx context.Context, st *store.Store, importer *core.Importer, im *model.Import, jsonOut bool) int {
	res, err := importer.Run(ctx, im.ID)
	if err != nil {
		return fail(err)
	}
	latest, err := st.Import(ctx, im.ID)
	if err != nil {
		return fail(err)
	}
	status, err := importer.Status(ctx, im.ID)
	if err != nil {
		return fail(err)
	}
	if jsonOut {
		printJSON(map[string]any{"import": latest, "result": res, "alerts": status.Events})
	} else {
		fmt.Printf("status=%s files=%d local_verified=%d failed=%d\n",
			latest.Status, res.Total, res.LocalVerified, res.Failed)
		if res.Paused {
			fmt.Printf("paused: %s\n", res.Message)
		}
		for _, e := range status.Events {
			fmt.Printf("alert [%s] %s\n", e.Level, e.Message)
		}
	}
	switch {
	case res.Paused:
		return exitPaused
	case latest.Status == model.ImportComplete || latest.Status == model.ImportLocalComplete:
		if res.Failed > 0 {
			return exitIncomplete
		}
		return exitOK
	case res.Failed > 0:
		return exitIncomplete
	}
	return exitIncomplete
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	id := fs.String("id", "", "import id")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	dbPath := fs.String("db", defaultDBPath(), "state database path")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *id == "" {
		fmt.Fprintln(os.Stderr, "status requires -id")
		return exitError
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fail(err)
	}
	defer st.Close()
	importer := core.New(st, core.Options{})
	status, err := importer.Status(ctx, *id)
	if err != nil {
		return fail(err)
	}
	if *jsonOut {
		printJSON(status)
		return exitOK
	}
	im := status.Import
	fmt.Printf("import   %s\n", im.ID)
	fmt.Printf("project  %s  date %s\n", im.ProjectCode, im.ShootDate)
	fmt.Printf("source   %s\n", im.SourceRoot)
	fmt.Printf("dest A   %s\n", im.DestA)
	if im.RequireBackup {
		fmt.Printf("dest B   %s\n", im.DestB)
	} else {
		fmt.Printf("dest B   (disabled)\n")
	}
	fmt.Printf("status   %s%s\n", im.Status, pausedSuffix(im.PauseReason))
	fmt.Printf("files    %d total, %d local verified, %d remote verified, %d failed\n",
		status.Counts.Total, status.Counts.LocalVerified, status.Counts.RemoteVerified, status.Counts.Failed)
	for _, e := range status.Events {
		fmt.Printf("alert    [%s] %s\n", e.Level, e.Message)
	}
	return exitOK
}

func cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "maximum imports to show")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	dbPath := fs.String("db", defaultDBPath(), "state database path")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fail(err)
	}
	defer st.Close()
	imports, err := st.Imports(ctx, *limit)
	if err != nil {
		return fail(err)
	}
	if *jsonOut {
		printJSON(imports)
		return exitOK
	}
	if len(imports) == 0 {
		fmt.Println("no imports")
		return exitOK
	}
	for _, im := range imports {
		counts, err := st.FileCounts(ctx, im.ID)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("%s  %-14s %-16s %-10s %d/%d local%s\n",
			im.ID, im.Status, im.ProjectCode, im.ShootDate,
			counts.LocalVerified, counts.Total, pausedSuffix(im.PauseReason))
	}
	return exitOK
}

func cmdCards(args []string) int {
	fs := flag.NewFlagSet("cards", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	vols, err := card.Detect(context.Background())
	if err != nil {
		return fail(err)
	}
	if *jsonOut {
		printJSON(vols)
		return exitOK
	}
	if len(vols) == 0 {
		fmt.Println("no removable volumes detected")
		return exitOK
	}
	for _, v := range vols {
		fmt.Printf("%s  %-20s %s  capacity %s  fingerprint %s\n",
			v.Root, card.DisplayLabel(v.Label), v.GUID, bytesHuman(v.Capacity), v.Fingerprint())
	}
	return exitOK
}

func cmdUpdateCheck(args []string) int {
	fs := flag.NewFlagSet("update-check", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "manifest URL (defaults to the build configuration)")
	keyB64 := fs.String("key", "", "base64 Ed25519 public key (defaults to the build configuration)")
	channel := fs.String("channel", "", "release channel override")
	signer := fs.String("signer", "", "pinned signer thumbprint (required for stable)")
	current := fs.String("current", api.Version, "version to compare against")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return exitError
	}

	cfg, err := updater.ResolveConfig("")
	if err != nil && !errors.Is(err, updater.ErrNotConfigured) {
		return fail(err)
	}
	if *manifest != "" || *keyB64 != "" || *channel != "" || *signer != "" {
		key := *keyB64
		if key == "" && len(cfg.PublicKey) == ed25519.PublicKeySize {
			key = base64.StdEncoding.EncodeToString(cfg.PublicKey)
		}
		url, ch, pin := *manifest, *channel, *signer
		if url == "" {
			url = cfg.ManifestURL
		}
		if ch == "" {
			ch = cfg.Channel
		}
		if pin == "" {
			pin = cfg.SignerThumbprint
		}
		cfg, err = updater.NewConfig(url, key, ch, pin, "")
		if err != nil {
			return fail(err)
		}
	}
	if err != nil {
		return fail(err)
	}

	res, err := cfg.Client().Check(context.Background(), *current)
	if err != nil {
		return fail(err)
	}
	if *jsonOut {
		printJSON(map[string]any{
			"current": *current, "available": res.Available,
			"mandatory": res.Mandatory, "manifest": res.Manifest,
		})
	} else if res.Available {
		fmt.Printf("update available: %s -> %s", *current, res.Manifest.Version)
		if res.Mandatory {
			fmt.Print(" (mandatory)")
		}
		fmt.Println()
		if res.Manifest.Notes != "" {
			fmt.Println(res.Manifest.Notes)
		}
		fmt.Printf("%s\n", res.Manifest.URL)
	} else {
		fmt.Printf("up to date: %s\n", *current)
	}
	if res.Available {
		return exitIncomplete
	}
	return exitOK
}

func cmdSelftest(args []string) int {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	report := selftest.Run(context.Background())
	if *jsonOut {
		printJSON(report)
	} else {
		for _, s := range report.Steps {
			mark := "ok"
			if !s.OK {
				mark = "FAIL"
			}
			fmt.Printf("[%s] %-34s %5d ms", mark, s.Name, s.DurationMS)
			if s.Detail != "" {
				fmt.Printf("  %s", s.Detail)
			}
			fmt.Println()
		}
	}
	if !report.OK {
		return exitError
	}
	return exitOK
}

func cmdLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	server := fs.String("server", "", "server URL (https://...)")
	email := fs.String("email", "", "account email")
	password := fs.String("password", "", "account password")
	tenant := fs.String("tenant", "", "tenant slug (only when one email spans tenants)")
	dbPath := fs.String("db", defaultDBPath(), "state database path")
	settingsPath := fs.String("settings", "", "settings file (defaults next to the database)")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	path := resolveSettingsPath(*settingsPath, *dbPath)
	s, err := settings.Load(path)
	if err != nil {
		return fail(err)
	}
	if *server != "" {
		s.ServerURL = *server
	}
	if *tenant != "" {
		s.TenantSlug = *tenant
	}
	if s.ServerURL == "" {
		fmt.Fprintln(os.Stderr, "login requires -server URL (or a settings file with server_url)")
		return exitError
	}
	if *email == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "login requires -email and -password")
		return exitError
	}
	conn, err := remote.Open(remote.Config{
		ServerURL: s.ServerURL, TenantSlug: s.TenantSlug, BandwidthKbps: s.BandwidthKbps,
		Dir: filepath.Dir(path),
	})
	if err != nil {
		return fail(err)
	}
	ctx := context.Background()
	if _, err := conn.Login(ctx, *email, *password); err != nil {
		return fail(remote.LoginError(err))
	}
	dev, err := conn.Register(ctx)
	if err != nil {
		return fail(err)
	}
	// Persist the endpoint only after a successful sign-in so later commands
	// (upload, resume) share the connection without repeating -server.
	if _, err := settings.SaveConnection(path, s.ServerURL, s.TenantSlug, s.BandwidthKbps); err != nil {
		return fail(err)
	}
	if *jsonOut {
		printJSON(map[string]any{"info": conn.Describe(), "device": dev})
	} else {
		info := conn.Describe()
		fmt.Printf("signed in as %s (%s) on %s\n", info.Email, info.Role, info.TenantName)
		fmt.Printf("device %s  fingerprint %s\n", dev.ID, info.Fingerprint)
	}
	return exitOK
}

func cmdUpload(args []string) int {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	id := fs.String("id", "", "import id (defaults to the latest locally complete import)")
	server := fs.String("server", "", "override the settings server URL")
	bandwidth := fs.Int("bandwidth", -1, "upload limit in kbit/s (0 = unlimited)")
	workers := fs.Int("workers", 2, "parallel upload workers")
	dbPath := fs.String("db", defaultDBPath(), "state database path")
	settingsPath := fs.String("settings", "", "settings file (defaults next to the database)")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	path := resolveSettingsPath(*settingsPath, *dbPath)
	s, err := settings.Load(path)
	if err != nil {
		return fail(err)
	}
	if *server != "" {
		s.ServerURL = *server
	}
	if *bandwidth >= 0 {
		s.BandwidthKbps = *bandwidth
	}
	if s.ServerURL == "" {
		fmt.Fprintln(os.Stderr, "upload requires -server URL (or a settings file with server_url)")
		return exitError
	}

	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fail(err)
	}
	defer st.Close()

	im, err := pickImport(ctx, st, *id)
	if err != nil {
		return fail(err)
	}
	conn, err := remote.Open(remote.Config{
		ServerURL: s.ServerURL, TenantSlug: s.TenantSlug, BandwidthKbps: s.BandwidthKbps,
		Dir: filepath.Dir(path),
	})
	if err != nil {
		return fail(err)
	}
	if !conn.LoggedIn() {
		fmt.Fprintln(os.Stderr, "not signed in: run `pwf-agent login` first")
		return exitError
	}
	deviceID, err := conn.EnsureDevice(ctx)
	if err != nil {
		return fail(err)
	}
	deriver := newDeriver(st, *dbPath)
	defer deriver.Close()
	up := upload.New(st, conn.API(), upload.Options{
		DeviceID: deviceID,
		Workers:  *workers,
		Previews: deriver,
		OnEvent: func(e model.Event) {
			if !*jsonOut {
				fmt.Printf("event [%s] %s\n", e.Level, e.Message)
			}
		},
	})
	res, err := up.Run(ctx, im.ID)
	if err != nil {
		return fail(err)
	}
	latest, lerr := st.Import(ctx, im.ID)
	if lerr != nil {
		return fail(lerr)
	}
	if *jsonOut {
		printJSON(res)
	} else {
		fmt.Printf("status=%s total=%d uploaded=%d remote_verified=%d failed=%d pending=%d derivatives=%d/%d\n",
			latest.Status, res.Total, res.Uploaded, res.RemoteVerified, res.Failed, res.Pending,
			res.Derivatives, res.DerivativesFailed)
		if res.Paused {
			fmt.Printf("paused: %s (%s)\n", res.Reason, res.Message)
		}
	}
	switch {
	case res.Paused:
		return exitPaused
	case res.Failed > 0:
		return exitIncomplete
	case res.Total > 0 && res.RemoteVerified == res.Total:
		return exitOK
	}
	return exitIncomplete
}

func pickImport(ctx context.Context, st *store.Store, id string) (*model.Import, error) {
	if id != "" {
		return st.Import(ctx, id)
	}
	imports, err := st.Imports(ctx, 50)
	if err != nil {
		return nil, err
	}
	for i := range imports {
		im := imports[i]
		if im.ProjectID == "" {
			continue
		}
		if im.Status == model.ImportLocalComplete || im.Status == model.ImportUploading {
			return &im, nil
		}
	}
	return nil, errors.New("no locally complete import waiting for upload")
}

// resolveSettingsPath keeps CLI state next to the database so a custom -db
// (used by tests and CI) also relocates the session and device identity.
func resolveSettingsPath(explicit, dbPath string) string {
	if explicit != "" {
		return explicit
	}
	return filepath.Join(filepath.Dir(dbPath), "settings.json")
}

func pausedSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (paused: " + reason + ")"
}

func bytesHuman(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "json: %v\n", err)
	}
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return exitError
}
