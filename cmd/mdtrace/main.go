// Command mdtrace records Mission Day mission completion counts and ratings from Ingress Intel.
//
//	mdtrace collect        fetch every mission once and store a snapshot (run by launchd every 5 min)
//	mdtrace export         write all snapshots as CSV
//	mdtrace telegram-test  send a test message with the configured Telegram bot
//	mdtrace weather-test   fetch and print the current weather with the configured WeatherKit key
//	mdtrace final          send the end-of-event summary with the Mission Day replay (run when collection ends)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mdtrace/internal/intel"
	"mdtrace/internal/missions"
	"mdtrace/internal/store"
	"mdtrace/internal/telegram"
	"mdtrace/internal/weather"
)

const usage = `usage:
  mdtrace collect       [-dir DIR] [-cookies FILE] [-delay 2.5s] [-notify] [-tz Asia/Shanghai]
                        [-charts charts.R] [-rscript Rscript]
  mdtrace export        [-dir DIR] [-o FILE] [-weather FILE] [-tz Asia/Shanghai]
  mdtrace telegram-test [-dir DIR] [-tz Asia/Shanghai] [-charts charts.R]
  mdtrace weather-test  [-dir DIR] [-tz Asia/Shanghai]
  mdtrace final         [-dir DIR] [-tz Asia/Shanghai] [-charts charts.R] [-preview]

DIR (default ~/.config/mdtrace) holds missions.json, mdtrace.db and, optionally,
telegram.env (TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID) for Telegram updates and alerts,
and weatherkit.env (WEATHERKIT_TEAM_ID, WEATHERKIT_KEY_ID, WEATHERKIT_SERVICE_ID,
WEATHERKIT_LAT, WEATHERKIT_LNG; key in weatherkit.p8) for weather in updates and charts.
The Intel session comes from FILE (default DIR/intel.cookie): the Cookie request
header of a logged-in https://intel.ingress.com tab, copied from the browser's
developer tools. It is re-read on every run, so replacing the file renews the session.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "collect":
		err = runCollect(ctx, os.Args[2:])
	case "export":
		err = runExport(ctx, os.Args[2:])
	case "telegram-test":
		err = runTelegramTest(ctx, os.Args[2:])
	case "weather-test":
		err = runWeatherTest(ctx, os.Args[2:])
	case "final":
		err = runFinal(ctx, os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mdtrace:", err)
		os.Exit(1)
	}
}

func defaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".mdtrace"
	}
	return filepath.Join(home, ".config", "mdtrace")
}

func runCollect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	dir := fs.String("dir", defaultDir(), "data directory")
	delay := fs.Duration("delay", 2500*time.Millisecond, "pause between Intel requests")
	cookies := fs.String("cookies", "", "file holding the Cookie header of a logged-in Intel session (default DIR/intel.cookie)")
	notify := fs.Bool("notify", false, "show a macOS notification when the Intel session has expired")
	tz := fs.String("tz", "Local", "IANA time zone for Telegram message times")
	chartsScript := fs.String("charts", "", "path to charts.R; when set, Telegram updates include charts")
	rscript := fs.String("rscript", "Rscript", "Rscript executable used to render charts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cookies == "" {
		*cookies = filepath.Join(*dir, "intel.cookie")
	}
	if *delay < 500*time.Millisecond {
		return fmt.Errorf("-delay must be at least 500ms")
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return fmt.Errorf("invalid -tz: %w", err)
	}

	logger := log.New(os.Stdout, "", log.LstdFlags)
	list, err := missions.Load(filepath.Join(*dir, "missions.json"))
	if err != nil {
		return err
	}

	db, err := store.Open(filepath.Join(*dir, "mdtrace.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	n := newNotifier(*dir, db, loc, list, logger)
	wx, err := loadWeather(*dir)
	if err != nil {
		logger.Printf("weather disabled: %v", err)
	}
	if n != nil && *chartsScript != "" {
		if path, err := exec.LookPath(*rscript); err != nil {
			logger.Printf("charts disabled: %v", err)
		} else {
			n.charts = &rCharts{rscript: path, script: *chartsScript, geoDir: geoDir(*dir), rows: db, weather: db, loc: loc, timeout: 2 * time.Minute}
		}
	}
	report := func(res collectResult, runErr error) {
		if n == nil {
			return
		}
		if err := n.afterRun(ctx, res, runErr); err != nil {
			logger.Printf("telegram: %v", err)
		}
	}

	cookie, err := readCookieHeader(*cookies)
	var fetcher *intel.Client
	if err == nil {
		fetcher, err = intel.NewClient(&http.Client{Timeout: 30 * time.Second}, intel.DefaultBaseURL, cookie, "")
	}
	if err != nil {
		// Record the attempt so gaps in the time series are explained in the database.
		runID, startErr := db.StartRun(ctx, len(list), time.Now())
		if startErr == nil {
			startErr = db.FinishRun(ctx, runID, 0, err)
		}
		report(collectResult{RunID: runID}, err)
		return handleCollectError(errors.Join(err, startErr), *notify)
	}
	c := &collector{fetcher: fetcher, store: db, logger: logger, delay: *delay, sleep: sleepContext, now: time.Now}
	logger.Printf("run start: %d missions", len(list))
	res, err := c.run(ctx, list)
	logger.Printf("run %d done: %d ok, %d failed, %d skipped", res.RunID, res.OK, res.Failed, res.Skipped)
	if wx != nil && res.RunID != 0 {
		wxErr := recordWeather(ctx, wx, db, res.RunID, time.Now())
		if wxErr != nil {
			logger.Printf("weather: %v", wxErr)
		}
		if n != nil {
			if err := n.weatherStatus(ctx, wxErr); err != nil {
				logger.Printf("telegram: %v", err)
			}
		}
	}
	report(res, err)
	return handleCollectError(err, *notify)
}

// newNotifier returns nil when Telegram is not configured; a broken config is logged, not fatal,
// so collection keeps running.
func newNotifier(dir string, db *store.Store, loc *time.Location, list []missions.Mission, logger *log.Logger) *notifier {
	chat, channel, err := loadTelegram(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		logger.Printf("telegram disabled: %v", err)
		return nil
	}
	n := newPreviewNotifier(chat, db, loc, list)
	if channel != nil { // a nil *Client stored in the interface would not compare equal to nil
		n.channel = channel
	}
	return n
}

// loadTelegram returns the private-chat client and, if configured, the channel client (else nil).
func loadTelegram(dir string) (chat, channel *telegram.Client, err error) {
	cfg, err := telegram.LoadConfig(filepath.Join(dir, "telegram.env"))
	if err != nil {
		return nil, nil, err
	}
	// Updates upload chart images, which can take a while on a slow link.
	httpClient := &http.Client{Timeout: 90 * time.Second}
	if chat, err = telegram.NewClient(httpClient, telegram.DefaultBaseURL, cfg.Token, cfg.ChatID); err != nil {
		return nil, nil, err
	}
	if cfg.ChannelID != "" {
		if channel, err = telegram.NewClient(httpClient, telegram.DefaultBaseURL, cfg.Token, cfg.ChannelID); err != nil {
			return nil, nil, err
		}
	}
	return chat, channel, nil
}

func runTelegramTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("telegram-test", flag.ContinueOnError)
	dir := fs.String("dir", defaultDir(), "data directory")
	tz := fs.String("tz", "Local", "IANA time zone for message times")
	chartsScript := fs.String("charts", "", "path to charts.R to include charts")
	rscript := fs.String("rscript", "Rscript", "Rscript executable used to render charts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return fmt.Errorf("invalid -tz: %w", err)
	}
	client, _, err := loadTelegram(*dir)
	if err != nil {
		return err
	}
	list, err := missions.Load(filepath.Join(*dir, "missions.json"))
	if err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(*dir, "mdtrace.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	runID, ok, err := db.LatestRun(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return client.Send(ctx, "✅ MDTrace is connected. No data collected yet.")
	}
	n := newPreviewNotifier(client, db, loc, list)
	var charts []string
	if *chartsScript != "" {
		path, err := exec.LookPath(*rscript)
		if err != nil {
			return err
		}
		r := &rCharts{rscript: path, script: *chartsScript, geoDir: geoDir(*dir), rows: db, weather: db, loc: loc, timeout: 2 * time.Minute}
		paths, cleanup, err := r.Render(ctx)
		if err != nil {
			return err
		}
		defer cleanup()
		charts = paths
	}
	report, err := n.previewReport(ctx, runID)
	if err != nil {
		return err
	}
	// No plain-text fallback here: the test must surface any rich message rejection.
	return client.SendRich(ctx, report.richBlocks(charts))
}

// loadWeather returns nil without an error when WeatherKit is not configured.
func loadWeather(dir string) (*weather.Client, error) {
	cfg, err := weather.LoadConfig(filepath.Join(dir, "weatherkit.env"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return weather.NewClient(&http.Client{Timeout: 20 * time.Second}, weather.DefaultBaseURL, cfg)
}

// Each fetch re-reads the past day so the stored hourly history follows WeatherKit's revisions,
// and the coming hours so updates can show the forecast.
const (
	weatherHistory  = 24 * time.Hour
	weatherForecast = 7 * time.Hour
)

func recordWeather(ctx context.Context, wx *weather.Client, db *store.Store, runID int64, now time.Time) error {
	hour := now.Truncate(time.Hour)
	report, err := wx.Fetch(ctx, now, hour.Add(-weatherHistory), hour.Add(weatherForecast))
	if err != nil {
		return err
	}
	return db.SaveWeather(ctx, runID, report)
}

func runWeatherTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("weather-test", flag.ContinueOnError)
	dir := fs.String("dir", defaultDir(), "data directory")
	tz := fs.String("tz", "Local", "IANA time zone for printed times")
	if err := fs.Parse(args); err != nil {
		return err
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return fmt.Errorf("invalid -tz: %w", err)
	}
	wx, err := loadWeather(*dir)
	if err != nil {
		return err
	}
	if wx == nil {
		return fmt.Errorf("%s not found", filepath.Join(*dir, "weatherkit.env"))
	}
	now := time.Now()
	hour := now.Truncate(time.Hour)
	report, err := wx.Fetch(ctx, now, hour.Add(-weatherHistory), hour.Add(weatherForecast))
	if err != nil {
		return err
	}
	var upcoming []weather.Hour
	for _, h := range report.Hours {
		if h.Start.After(report.Current.AsOf) && len(upcoming) < weatherUpcomingHours {
			upcoming = append(upcoming, h)
		}
	}
	fmt.Println(weatherSummary(report.Current))
	fmt.Println(weatherOutlook(upcoming, loc))
	fmt.Printf("as of %s · %d hourly values\n", report.Current.AsOf.In(loc).Format("01-02 15:04"), len(report.Hours))
	return nil
}

// geoDir returns the map chart inputs directory inside dir, or "" when they are not installed.
func geoDir(dir string) string {
	path := filepath.Join(dir, "geo")
	for _, name := range []string{"routes.json", "coastline.json"} {
		if _, err := os.Stat(filepath.Join(path, name)); err != nil {
			return ""
		}
	}
	return path
}

func newPreviewNotifier(sender messageSender, db *store.Store, loc *time.Location, list []missions.Mission) *notifier {
	names := make(map[string]string, len(list))
	for _, m := range list {
		names[m.GUID] = m.DisplayName
	}
	return &notifier{sender: sender, store: db, loc: loc, now: time.Now, names: names}
}

// readCookieHeader is re-run on every collection so a replaced cookie file is picked up.
// Failures are reported as intel.ErrAuth because they all mean "no usable session".
func readCookieHeader(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", intel.ErrAuth, err)
	}
	return string(raw), nil
}

func handleCollectError(err error, notify bool) error {
	if err != nil && notify && errors.Is(err, intel.ErrAuth) {
		message := "Intel session expired. Copy a fresh Cookie header from intel.ingress.com into the cookie file."
		script := fmt.Sprintf(`display notification %q with title "MDTrace" sound name "Basso"`, message)
		if notifyErr := exec.Command("osascript", "-e", script).Run(); notifyErr != nil {
			err = errors.Join(err, fmt.Errorf("notification failed: %w", notifyErr))
		}
	}
	return err
}

func runExport(ctx context.Context, args []string) (err error) {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	dir := fs.String("dir", defaultDir(), "data directory")
	out := fs.String("o", "-", "output CSV file ('-' for stdout)")
	weatherOut := fs.String("weather", "", "also write the hourly weather series to this CSV file")
	tz := fs.String("tz", "Local", "IANA time zone for exported timestamps, e.g. Asia/Shanghai")
	if err := fs.Parse(args); err != nil {
		return err
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return fmt.Errorf("invalid -tz: %w", err)
	}

	dbPath := filepath.Join(*dir, "mdtrace.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no database at %s: %w", dbPath, err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.Rows(ctx)
	if err != nil {
		return err
	}

	var w io.Writer = os.Stdout
	if *out != "-" && strings.TrimSpace(*out) != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}()
		w = f
	}
	if err := writeCSV(w, rows, loc); err != nil {
		return err
	}
	if *out != "-" {
		fmt.Fprintf(os.Stderr, "wrote %d rows to %s\n", len(rows), *out)
	}
	if *weatherOut != "" {
		hours, err := db.WeatherHours(ctx)
		if err != nil {
			return err
		}
		if err := writeFile(*weatherOut, func(w io.Writer) error { return writeWeatherCSV(w, hours, loc) }); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %d weather hours to %s\n", len(hours), *weatherOut)
	}
	return nil
}
