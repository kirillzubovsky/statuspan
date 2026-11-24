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
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kirillzubovsky/statuspan/internal/relay"
	"github.com/kirillzubovsky/statuspan/internal/server"
	"github.com/kirillzubovsky/statuspan/internal/store"
)

func runCmd(args []string) error {
	fs := newFlagSet("run")
	agent := fs.String("agent", envOr("STATUSPAN_AGENT", ""), "agent name (default STATUSPAN_AGENT)")
	job := fs.String("job", "", "job id")
	title := fs.String("title", "", "title (default: the job id)")
	ttl := fs.Int("ttl", 600, "stall ttl in seconds")
	heartbeat := fs.Int("heartbeat", 0, "heartbeat interval in seconds (default min(ttl/2, 60) with a 10s floor)")
	link := fs.String("link", "", "link URL")
	recurring := fs.Bool("recurring", false, "post nothing on success; post failed on a non-zero exit")
	jobURL := fs.String("url", "", "full jobs endpoint (overrides --addr; env STATUSPAN_URL)")
	addr := fs.String("addr", envOr("STATUSPAN_ADDR", server.DefaultAddr), "server address")
	relayURL := fs.String("relay", "", "post to this relay instead of the local daemon")
	workspace := fs.String("workspace", "", "relay workspace")
	token := fs.String("token", "", "relay write token")
	tokenFile := fs.String("token-file", "", "file containing the relay write token")
	verbose := fs.Bool("verbose", false, "print StatusPan warnings on stderr")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &exitError{code: 2, err: err}
	}
	cmdArgs := fs.Args()
	agentName := strings.TrimSpace(*agent)
	jobID := strings.TrimSpace(*job)
	if agentName == "" || jobID == "" || len(cmdArgs) == 0 {
		return &exitError{code: 2, err: fmt.Errorf("run requires --agent, --job, and a command")}
	}
	if *ttl < 0 {
		return &exitError{code: 2, err: fmt.Errorf("ttl must be >= 0")}
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["heartbeat"] && *heartbeat < 1 {
		return &exitError{code: 2, err: fmt.Errorf("heartbeat must be >= 1")}
	}
	endpointURL, rTok, reportErr := resolveRunEndpoint(seen, *jobURL, *addr, *relayURL, *workspace, *token, *tokenFile)
	titleText := *title
	if titleText == "" {
		titleText = jobID
	}
	every := defaultHeartbeat(*ttl)
	if seen["heartbeat"] {
		every = time.Duration(*heartbeat) * time.Second
	}
	jobSpec := runJob{
		agent: agentName,
		jobID: jobID,
		title: titleText,
		link:  strings.TrimSpace(*link),
		ttl:   *ttl,
	}
	var p *poster
	if reportErr != nil {
		// Reporting setup must not block the wrapped command.
		warnConfig(reportErr, configSecrets(seen, *token))
	} else {
		p = &poster{
			url:     endpointURL,
			token:   rTok,
			verbose: *verbose,
			client:  &http.Client{Timeout: 2 * time.Second},
		}
	}

	var ap *asyncPoster
	if p != nil && !*recurring {
		ap = newAsyncPoster(p)
		zero := 0
		ap.fire(jobSpec.update("running", &zero, ""))
	}

	// Leave the child in this process group. A new group is not the
	// terminal foreground, so an interactive command stops on SIGTTIN
	// or SIGTTOU.
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// A terminal delivers SIGINT to the whole foreground group, which
	// already includes the child. Forwarding it again would deliver it
	// twice. SIGTERM and SIGHUP are forwarded because they were sent to
	// statuspan, not to that group.
	forwardINT := !stdinIsCharDevice()
	sigCtx, stopSig := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	sigDone := make(chan struct{})
	defer func() {
		stopSig()
		signal.Stop(sigCh)
		<-sigDone
	}()

	started := time.Now()
	if err := cmd.Start(); err != nil {
		close(sigDone)
		if ap != nil {
			ap.stop()
		}
		note := "Could not start: " + err.Error()
		if p != nil {
			p.warn(p.send(jobSpec.update("failed", nil, note)))
		}
		return &exitError{code: 127, err: errors.New(note)}
	}
	go func() {
		defer close(sigDone)
		for {
			select {
			case <-sigCtx.Done():
				return
			case sig := <-sigCh:
				if !forwardINT && isSIGINT(sig) {
					continue
				}
				_ = signalProcess(cmd, sig)
			}
		}
	}()

	hbCtx, hbCancel := context.WithCancel(context.Background())
	hbDone := make(chan struct{})
	if p != nil && !*recurring {
		go func() {
			defer close(hbDone)
			ticker := time.NewTicker(every)
			defer ticker.Stop()
			for {
				select {
				case <-hbCtx.Done():
					return
				case <-ticker.C:
					zero := 0
					ap.fire(jobSpec.update("running", &zero, ""))
				}
			}
		}()
	} else {
		close(hbDone)
	}

	waitErr := cmd.Wait()
	hbCancel()
	<-hbDone
	if ap != nil {
		ap.stop()
	}

	code := commandExitCode(waitErr)
	if p == nil || (*recurring && code == 0) {
		if code == 0 {
			return nil
		}
		return &exitError{code: code}
	}
	dur := formatRunDur(time.Since(started))
	if code == 0 {
		hundred := 100
		p.warn(p.send(jobSpec.update("done", &hundred, "Completed in "+dur)))
		return nil
	}
	note := fmt.Sprintf("Exited with code %d after %s", code, dur)
	p.warn(p.send(jobSpec.update("failed", nil, note)))
	return &exitError{code: code}
}

// resolveRunEndpoint picks the jobs URL. An error is a reporting
// misconfiguration: the caller warns and still runs the command.
func resolveRunEndpoint(seen map[string]bool, urlFlag, addr, relayURL, workspace, token, tokenFile string) (string, string, error) {
	remote, rURL, rWS, rTok, err := relayTarget(seen, relayURL, workspace, token, tokenFile)
	if err != nil {
		return "", "", err
	}
	if remote && seen["url"] {
		return "", "", fmt.Errorf("--url cannot be combined with a relay post")
	}
	if remote {
		if err := validateJobsEndpoint(rURL); err != nil {
			return "", "", fmt.Errorf("--relay: %w", err)
		}
		return relayJobsEndpoint(rURL, rWS), rTok, nil
	}
	endpointURL, err := localJobsURL(seen, urlFlag, addr)
	if err != nil {
		return "", "", err
	}
	return endpointURL, "", nil
}

func warnConfig(err error, secrets []string) {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, "[redacted]")
	}
	fmt.Fprintf(os.Stderr, "statuspan: warning: %s\n", msg)
}

func configSecrets(seen map[string]bool, flagToken string) []string {
	var secrets []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, existing := range secrets {
			if existing == s {
				return
			}
		}
		secrets = append(secrets, s)
	}
	if seen["token"] {
		add(flagToken)
	}
	add(os.Getenv("STATUSPAN_TOKEN"))
	return secrets
}

func stdinIsCharDevice() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func isSIGINT(sig os.Signal) bool {
	return sig == syscall.SIGINT || sig == os.Interrupt
}

type runJob struct {
	agent string
	jobID string
	title string
	link  string
	ttl   int
}

func (r runJob) update(state string, progress *int, note string) store.Update {
	seq := nextSeq()
	ttl := r.ttl
	title := r.title
	u := store.Update{
		Agent: r.agent,
		JobID: r.jobID,
		State: state,
		Title: &title,
		Seq:   &seq,
		TTL:   &ttl,
	}
	if progress != nil {
		n := *progress
		u.Progress = &n
	}
	if note != "" {
		n := note
		u.Note = &n
	}
	if r.link != "" {
		link := r.link
		u.Link = &link
	}
	return u
}

// defaultHeartbeat is min(ttl/2, 60s), and at least 10s.
func defaultHeartbeat(ttlSec int) time.Duration {
	half := ttlSec / 2
	if half > 60 {
		half = 60
	}
	if half < 10 {
		half = 10
	}
	return time.Duration(half) * time.Second
}

func formatRunDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Millisecond).String()
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return waitStatusCode(ee)
	}
	return 1
}

type poster struct {
	url     string
	token   string
	verbose bool
	client  *http.Client
}

func (p *poster) send(u store.Update) error {
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: %s: %s", req.URL.Path, resp.Status, relay.ErrorText(body))
	}
	return nil
}

func (p *poster) warn(err error) {
	if err == nil || !p.verbose {
		return
	}
	msg := err.Error()
	if p.token != "" {
		msg = strings.ReplaceAll(msg, p.token, "[redacted]")
	}
	fmt.Fprintf(os.Stderr, "statuspan: %s\n", msg)
}

// asyncPoster sends updates without blocking the wrapped command.
// stop waits for the in-flight post, then drops anything still queued.
type asyncPoster struct {
	p      *poster
	ch     chan store.Update
	done   chan struct{}
	mu     sync.Mutex
	closed bool
	once   sync.Once
}

func newAsyncPoster(p *poster) *asyncPoster {
	a := &asyncPoster{
		p:    p,
		ch:   make(chan store.Update, 1),
		done: make(chan struct{}),
	}
	go func() {
		defer close(a.done)
		for u := range a.ch {
			p.warn(p.send(u))
		}
	}()
	return a
}

func (a *asyncPoster) fire(u store.Update) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	select {
	case a.ch <- u:
	default:
		a.p.warn(errors.New("dropped status post"))
	}
}

func (a *asyncPoster) stop() {
	a.once.Do(func() {
		a.mu.Lock()
		a.closed = true
		close(a.ch)
		a.mu.Unlock()
	})
	<-a.done
}

func relayJobsEndpoint(base, workspace string) string {
	return strings.TrimRight(base, "/") + "/v1/w/" + url.PathEscape(workspace) + "/jobs"
}

func localJobsURL(seen map[string]bool, urlFlag, addr string) (string, error) {
	if seen["url"] {
		u := strings.TrimSpace(urlFlag)
		if u == "" {
			return "", fmt.Errorf("--url requires an endpoint")
		}
		if err := validateJobsEndpoint(u); err != nil {
			return "", fmt.Errorf("--url: %w", err)
		}
		return u, nil
	}
	if !seen["addr"] {
		if u := strings.TrimSpace(os.Getenv("STATUSPAN_URL")); u != "" {
			if err := validateJobsEndpoint(u); err != nil {
				return "", fmt.Errorf("STATUSPAN_URL: %w", err)
			}
			return u, nil
		}
	}
	return jobsURLFromAddr(addr)
}

func jobsURLFromAddr(addr string) (string, error) {
	raw := strings.TrimSpace(addr)
	if strings.Contains(raw, "://") {
		if err := validateJobsEndpoint(raw); err != nil {
			return "", fmt.Errorf("--addr: %w", err)
		}
		return endpoint(raw, "/api/jobs"), nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" || port == "" {
		if err == nil {
			err = fmt.Errorf("missing host or port")
		}
		return "", fmt.Errorf("--addr: %w", err)
	}
	u := endpoint(raw, "/api/jobs")
	if err := validateJobsEndpoint(u); err != nil {
		return "", fmt.Errorf("--addr: %w", err)
	}
	return u, nil
}

func validateJobsEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("must be an http or https URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if u.Hostname() == "" || (scheme != "http" && scheme != "https") {
		return fmt.Errorf("must be an http or https URL")
	}
	return nil
}
