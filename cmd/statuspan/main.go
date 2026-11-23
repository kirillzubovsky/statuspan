package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/kirillzubovsky/statuspan/internal/relay"
	"github.com/kirillzubovsky/statuspan/internal/server"
	"github.com/kirillzubovsky/statuspan/internal/store"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "statuspan: %s\n", err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return serveCmd(args)
	}
	switch args[0] {
	case "serve":
		return serveCmd(args[1:])
	case "post":
		return postCmd(args[1:])
	case "ls":
		return lsCmd(args[1:])
	case "clear":
		return clearCmd(args[1:])
	case "version", "-version", "--version":
		fmt.Println("statuspan", version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (try statuspan help)", args[0])
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `statuspan %s — local agent status board

Usage:
  statuspan [serve] [--addr host:port] [--data dir]
            [--abandon-after DUR] [--abandon-waiting-after DUR]
            [--relay-url URL] [--relay-workspace NAME] [--relay-token-file PATH]
  statuspan post --agent A --job ID [--state STATE] [--title T] [--progress N] [--link URL] [--note N] [--seq N] [--ttl S]
            [--addr host:port | --relay URL --workspace NAME (--token TOKEN | --token-file PATH)]
  statuspan ls [--addr host:port]
  statuspan clear --yes [--addr host:port]
  statuspan version

Relay sync is optional. With no relay flags, environment, or data-dir relay.json, the daemon stays local.
post talks to the local daemon unless --relay / --workspace / --token (or STATUSPAN_RELAY_URL, STATUSPAN_RELAY_WORKSPACE, STATUSPAN_TOKEN) are set.

`, version)
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		usage()
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	return fs
}

func serveCmd(args []string) error {
	fs := newFlagSet("serve")
	addr := fs.String("addr", envOr("STATUSPAN_ADDR", server.DefaultAddr), "listen address (loopback only)")
	data := fs.String("data", envOr("STATUSPAN_DATA", defaultDataDir()), "data directory")
	relayURL := fs.String("relay-url", "", "relay base URL (overrides STATUSPAN_RELAY_URL and relay.json)")
	relayWS := fs.String("relay-workspace", "", "relay workspace (overrides STATUSPAN_RELAY_WORKSPACE and relay.json)")
	relayTokenFile := fs.String("relay-token-file", "", "file containing the relay read token")
	abandonAfter := fs.String("abandon-after", envOr("STATUSPAN_ABANDON_AFTER", "24h"), "silence before a stalled running job is shown as abandoned (0 disables)")
	abandonWaiting := fs.String("abandon-waiting-after", envOr("STATUSPAN_ABANDON_WAITING_AFTER", "168h"), "silence before a queued or blocked job is shown as abandoned (0 disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := server.ValidateAddr(*addr); err != nil {
		return err
	}
	after, err := parseNonNegativeDuration("abandon-after", *abandonAfter)
	if err != nil {
		return err
	}
	waiting, err := parseNonNegativeDuration("abandon-waiting-after", *abandonWaiting)
	if err != nil {
		return err
	}
	st, err := store.OpenWith(*data, store.Options{
		AbandonAfter:        &after,
		AbandonWaitingAfter: &waiting,
	})
	if err != nil {
		return err
	}
	defer st.Close()

	syncer, err := relay.New(relay.Options{
		Store:         st,
		DataDir:       *data,
		FlagURL:       *relayURL,
		FlagWorkspace: *relayWS,
		FlagTokenFile: *relayTokenFile,
	})
	if err != nil {
		return err
	}

	handler := server.New(st)
	if syncer != nil {
		handler.SetRelay(syncer.Status)
	}
	srv := &http.Server{
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var wg sync.WaitGroup
	defer func() {
		stop()
		wg.Wait()
	}()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	fmt.Printf("statuspan listening on http://%s\n", ln.Addr().String())
	fmt.Printf("data directory %s\n", *data)
	if syncer != nil {
		fmt.Printf("relay sync %s workspace %s\n", syncer.BaseURL(), syncer.Workspace())
		wg.Add(1)
		go func() {
			defer wg.Done()
			syncer.Run(ctx)
		}()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func postCmd(args []string) error {
	fs := newFlagSet("post")
	agent := fs.String("agent", "", "agent name")
	job := fs.String("job", "", "job id")
	state := fs.String("state", "", "queued, running, blocked, done, or failed (required on a new job)")
	title := fs.String("title", "", "title")
	link := fs.String("link", "", "link URL")
	note := fs.String("note", "", "note")
	addr := fs.String("addr", envOr("STATUSPAN_ADDR", server.DefaultAddr), "server address")
	relayURL := fs.String("relay", "", "post to this relay instead of the local daemon")
	workspace := fs.String("workspace", "", "relay workspace")
	token := fs.String("token", "", "relay write token")
	tokenFile := fs.String("token-file", "", "file containing the relay write token")
	var progress optInt
	var ttl optInt
	var seq optInt64
	fs.Var(&progress, "progress", "progress 0-100")
	fs.Var(&ttl, "ttl", "stall ttl in seconds")
	fs.Var(&seq, "seq", "monotonic sequence; older or equal values are ignored")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *agent == "" || *job == "" {
		return fmt.Errorf("post requires --agent and --job")
	}
	u := store.Update{Agent: *agent, JobID: *job, State: *state}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["title"] {
		u.Title = title
	}
	if seen["link"] {
		u.Link = link
	}
	if seen["note"] {
		u.Note = note
	}
	if progress.set {
		if progress.n < 0 || progress.n > 100 {
			return fmt.Errorf("progress must be an integer from 0 to 100")
		}
		u.Progress = &progress.n
	}
	if ttl.set {
		if ttl.n < 0 {
			return fmt.Errorf("ttl must be >= 0")
		}
		u.TTL = &ttl.n
	}
	if seq.set {
		u.Seq = &seq.n
	}
	if err := store.Validate(u); err != nil {
		return err
	}
	remote, rURL, rWS, rTok, err := relayTarget(seen, *relayURL, *workspace, *token, *tokenFile)
	if err != nil {
		return err
	}
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	if remote {
		return postRelay(rURL, rWS, rTok, data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(*addr, "/api/jobs"), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Printf("%s", body)
	if !bytes.HasSuffix(body, []byte("\n")) {
		fmt.Println()
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST /api/jobs: %s", resp.Status)
	}
	return nil
}

func relayTarget(seen map[string]bool, flagURL, flagWS, flagToken, flagFile string) (bool, string, string, string, error) {
	if seen["token"] && seen["token-file"] {
		return false, "", "", "", fmt.Errorf("pass only one of --token or --token-file")
	}
	urlVal := strings.TrimSpace(flagURL)
	if !seen["relay"] {
		urlVal = strings.TrimSpace(os.Getenv("STATUSPAN_RELAY_URL"))
	}
	ws := strings.TrimSpace(flagWS)
	if !seen["workspace"] {
		ws = strings.TrimSpace(os.Getenv("STATUSPAN_RELAY_WORKSPACE"))
	}
	var tok string
	switch {
	case seen["token"]:
		tok = strings.TrimSpace(flagToken)
	case seen["token-file"]:
		var err error
		tok, err = relay.ReadTokenFile(flagFile)
		if err != nil {
			return false, "", "", "", fmt.Errorf("token file: %w", err)
		}
	default:
		tok = strings.TrimSpace(os.Getenv("STATUSPAN_TOKEN"))
	}
	if urlVal == "" && ws == "" && tok == "" {
		return false, "", "", "", nil
	}
	var missing []string
	if urlVal == "" {
		missing = append(missing, "--relay or STATUSPAN_RELAY_URL")
	}
	if ws == "" {
		missing = append(missing, "--workspace or STATUSPAN_RELAY_WORKSPACE")
	}
	if tok == "" {
		missing = append(missing, "--token, --token-file, or STATUSPAN_TOKEN")
	}
	if len(missing) > 0 {
		return false, "", "", "", fmt.Errorf("relay post is missing %s", strings.Join(missing, ", "))
	}
	return true, strings.TrimRight(urlVal, "/"), ws, tok, nil
}

func postRelay(base, workspace, token string, data []byte) error {
	path := "/v1/w/" + url.PathEscape(workspace) + "/jobs"
	endpoint := strings.TrimRight(base, "/") + path
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("relay POST %s: %s: %s", req.URL.Path, resp.Status, relay.ErrorText(body))
	}
	fmt.Printf("%s", body)
	if !bytes.HasSuffix(body, []byte("\n")) {
		fmt.Println()
	}
	return nil
}

func lsCmd(args []string) error {
	fs := newFlagSet("ls")
	addr := fs.String("addr", envOr("STATUSPAN_ADDR", server.DefaultAddr), "server address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(*addr, "/api/jobs"), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /api/jobs: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	var snap struct {
		Jobs []store.JobView `json:"jobs"`
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		return err
	}
	if len(snap.Jobs) == 0 {
		fmt.Println("no jobs")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tJOB\tSTATE\tPROG\tELAPSED\tSTALLED\tTITLE\tNOTE")
	for _, j := range snap.Jobs {
		stalled := "-"
		switch {
		case j.Abandoned:
			stalled = "ABANDONED"
		case j.Stalled:
			stalled = "STALLED"
		}
		title := j.Title
		if title == "" {
			title = j.JobID
		}
		note := strings.Join(strings.Fields(j.Note), " ")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d%%\t%s\t%s\t%s\t%s\n",
			j.Agent, j.JobID, j.State, j.Progress, formatElapsed(j.ElapsedSeconds), stalled, title, note)
	}
	return tw.Flush()
}

func clearCmd(args []string) error {
	fs := newFlagSet("clear")
	addr := fs.String("addr", envOr("STATUSPAN_ADDR", server.DefaultAddr), "server address")
	yes := fs.Bool("yes", false, "delete every job and its history")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*yes {
		return fmt.Errorf("clear deletes every job; pass --yes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint(*addr, "/api/jobs?confirm=all"), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var out struct {
		OK      bool   `json:"ok"`
		Deleted int    `json:"deleted"`
		Backup  string `json:"backup"`
		Error   string `json:"error"`
	}
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		return fmt.Errorf("DELETE /api/jobs: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	if resp.StatusCode >= 300 || !out.OK {
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return fmt.Errorf("DELETE /api/jobs: %s: %s", resp.Status, msg)
	}
	fmt.Printf("deleted %d\n%s\n", out.Deleted, out.Backup)
	return nil
}

func formatElapsed(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func endpoint(addr, path string) string {
	base := strings.TrimRight(strings.TrimSpace(addr), "/")
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	return base + path
}

func parseNonNegativeDuration(name, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must be >= 0", name)
	}
	return d, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func defaultDataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return filepath.Join(".", "statuspan-data")
		}
		return filepath.Join(home, ".config", "statuspan")
	}
	return filepath.Join(dir, "statuspan")
}

type optInt struct {
	set bool
	n   int
}

func (o *optInt) String() string {
	if o == nil || !o.set {
		return ""
	}
	return strconv.Itoa(o.n)
}

func (o *optInt) Set(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	o.set = true
	o.n = n
	return nil
}

type optInt64 struct {
	set bool
	n   int64
}

func (o *optInt64) String() string {
	if o == nil || !o.set {
		return ""
	}
	return strconv.FormatInt(o.n, 10)
}

func (o *optInt64) Set(s string) error {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	o.set = true
	o.n = n
	return nil
}
