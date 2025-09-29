package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

/*
Features included:
- -i targets file, -o out dir
- --cmd-file or repeated -c
- Placeholders: {TARGET}, {STEP}, {STEPIDX}
- Per-target outputs: step-*.out/.err, combined.out/.err, commands.ran
- Concurrency -P (targets in parallel; steps sequential per target)
- -t per-step timeout (seconds)
- --stdin (pipe target to step stdin; then don't use {TARGET})
- --save-expanded, --aggregate, --err-aggregate
- --dry-run, --keep-order
ADDED:
- --tee (mirror stdout/stderr to console)
- --retries N and --backoff "min,max" (seconds) with exponential + jitter
- --stop-after-fail N (global step failures)
- --resume FILE (skip targets that previously completed ALL steps OK; append on success)
- --qps N (rate limit: cap step starts per second across workers)
- --unique (de-duplicate targets) and --shuffle (randomize targets order)
- --target (single target), and ability to run with ONLY --cmd-file:
  * If no -i/--target and no STDIN piped, run once with synthetic target "__run__" and do NOT require {TARGET}.
*/

var (
	inputFile       = flag.String("i", "", "Targets file (one per line)")
	outDir          = flag.String("o", "runlist_out", "Per-target output directory")
	cmdFile         = flag.String("cmd-file", "", "File with one command per line (# and blanks ignored)")
	multiCmds       multiStr
	concurrency     = flag.Int("P", 8, "Parallel targets")
	timeoutSec      = flag.Int("t", 0, "Per-step timeout seconds (0=none)")
	useStdin        = flag.Bool("stdin", false, "Pipe target to each command's STDIN (omit {TARGET} in commands)")
	keepOrder       = flag.Bool("keep-order", false, "Process targets serially (no parallelism)")
	dryRun          = flag.Bool("dry-run", false, "Print expanded commands and exit")
	saveExpanded    = flag.String("save-expanded", "", "Append every expanded command to this file")
	aggregateStdout = flag.String("aggregate", "", "Append all step stdout (all targets) to this file")
	aggregateStderr = flag.String("err-aggregate", "", "Append all step stderr (all targets) to this file")

	// New flags
	teeOutput     = flag.Bool("tee", false, "Mirror step stdout/stderr to console while saving to files")
	retries       = flag.Int("retries", 0, "Retries per failing step (total attempts = retries+1)")
	backoffRange  = flag.String("backoff", "1,60", "Backoff seconds as min,max (exponential with jitter)")
	stopAfterFail = flag.Int("stop-after-fail", 0, "Abort entire run after N step failures (global)")
	resumePath    = flag.String("resume", "", "Resume state file; skip targets already completed OK")
	qps           = flag.Float64("qps", 0, "Global start rate (queries per second) across workers")
	uniqueInput   = flag.Bool("unique", false, "De-duplicate targets before running")
	shuffleInput  = flag.Bool("shuffle", false, "Shuffle target list before running")

	// Optional single target (alternative to -i)
	singleTarget = flag.String("target", "", "Run against a single target (bypasses -i)")
)

type multiStr []string

func (m *multiStr) String() string     { return strings.Join(*m, " | ") }
func (m *multiStr) Set(v string) error { *m = append(*m, v); return nil }

var (
	reCRLF = regexp.MustCompile(`\r+$`)
	reSan  = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

func main() {
	rand.Seed(time.Now().UnixNano())
	flag.Var(&multiCmds, "c", "Command template (repeatable). Use {TARGET}. Extras: {STEP},{STEPIDX}")
	flag.Parse()

	// --- Load commands ---
	cmds := []string{}
	if *cmdFile != "" {
		lines, err := loadLines(*cmdFile)
		if err != nil {
			exitf("[!] reading --cmd-file: %v", err)
		}
		cmds = append(cmds, lines...)
	}
	cmds = append(cmds, multiCmds...)
	cmds = filterCmds(cmds)
	if len(cmds) == 0 {
		exitf("[!] no commands provided (use -c or --cmd-file)")
	}

	// --- Load targets (from --target, -i FILE, STDIN, or fallback single run) ---
	var targets []string
	noTargetMode := false // when true, commands are allowed to omit {TARGET}

	switch {
	case *singleTarget != "":
		targets = []string{strings.TrimSpace(*singleTarget)}

	case *inputFile != "":
		lines, err := loadLines(*inputFile)
		if err != nil {
			exitf("[!] reading targets: %v", err)
		}
		targets = filterTargets(lines)

	default:
		// If no -i and no --target, try to read from STDIN
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			// stdin is piped
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				targets = append(targets, sc.Text())
			}
			if err := sc.Err(); err != nil {
				exitf("[!] reading STDIN: %v", err)
			}
			targets = filterTargets(targets)
		} else {
			// No targets provided anywhere → run once without requiring {TARGET}
			targets = []string{"__run__"}
			noTargetMode = true
		}
	}

	// Hygiene
	if *uniqueInput {
		targets = dedupe(targets)
	}
	if *shuffleInput {
		rand.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
	}

	// Output dir
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		exitf("[!] creating outdir: %v", err)
	}

	// Aggregators
	var aggOut, aggErr *os.File
	var aggMu sync.Mutex
	if *aggregateStdout != "" {
		f, err := os.OpenFile(*aggregateStdout, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			exitf("[!] open aggregate stdout: %v", err)
		}
		aggOut = f
		defer f.Close()
	}
	if *aggregateStderr != "" {
		f, err := os.OpenFile(*aggregateStderr, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			exitf("[!] open aggregate stderr: %v", err)
		}
		aggErr = f
		defer f.Close()
	}

	// Save-expanded
	var saveExp *os.File
	if *saveExpanded != "" {
		f, err := os.OpenFile(*saveExpanded, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			exitf("[!] open save-expanded: %v", err)
		}
		saveExp = f
		defer f.Close()
	}

	// Resume map + writer
	resumeDone := map[string]bool{}
	var resumeMu sync.Mutex
	var resumeF *os.File
	if *resumePath != "" {
		resumeDone = loadResume(*resumePath)
		f, err := os.OpenFile(*resumePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			exitf("[!] open resume file: %v", err)
		}
		resumeF = f
		defer f.Close()
	}

	// QPS rate gate
	gate := newRateGate(*qps)

	// Global failure guard
	var failMu sync.Mutex
	failCount := 0
	incFail := func() (reached bool) {
		if *stopAfterFail <= 0 {
			return false
		}
		failMu.Lock()
		defer failMu.Unlock()
		failCount++
		return failCount >= *stopAfterFail
	}

	// Cancellation context for future soft-cancel hooks
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	total := len(targets)
	if *keepOrder {
		for _, t := range targets {
			if ctx.Err() != nil {
				break
			}
			// resume skip (not meaningful in noTargetMode)
			if *resumePath != "" && !noTargetMode && resumeDone[t] {
				continue
			}
			allOK := runTarget(ctx, t, cmds, saveExp, aggOut, aggErr, &aggMu, gate, incFail, noTargetMode)
			if allOK && *resumePath != "" && !noTargetMode {
				resumeMu.Lock()
				if !resumeDone[t] {
					fmt.Fprintln(resumeF, t)
					resumeDone[t] = true
				}
				resumeMu.Unlock()
			}
		}
	} else {
		sem := make(chan struct{}, max(1, *concurrency))
		var wg sync.WaitGroup
		for _, t := range targets {
			if *resumePath != "" && !noTargetMode && resumeDone[t] {
				continue
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(target string) {
				defer func() { <-sem; wg.Done() }()
				if ctx.Err() != nil {
					return
				}
				allOK := runTarget(ctx, target, cmds, saveExp, aggOut, aggErr, &aggMu, gate, incFail, noTargetMode)
				if allOK && *resumePath != "" && !noTargetMode {
					resumeMu.Lock()
					if !resumeDone[target] {
						fmt.Fprintln(resumeF, target)
						resumeDone[target] = true
					}
					resumeMu.Unlock()
				}
			}(t)
		}
		wg.Wait()
	}

	fmt.Printf("[summary] processed %d targets | outdir=%s\n", total, *outDir)
}

func runTarget(
	ctx context.Context,
	target string,
	cmds []string,
	saveExp, aggOut, aggErr *os.File,
	aggMu *sync.Mutex,
	gate *rateGate,
	incFail func() bool,
	noTargetMode bool,
) bool {
	safe := sanitize(target)
	tdir := filepath.Join(*outDir, safe)
	_ = os.MkdirAll(tdir, 0o755)

	combOutPath := filepath.Join(tdir, "combined.out")
	combErrPath := filepath.Join(tdir, "combined.err")
	cmdsRanPath := filepath.Join(tdir, "commands.ran")

	combOut, _ := os.Create(combOutPath)
	defer combOut.Close()
	combErr, _ := os.Create(combErrPath)
	defer combErr.Close()
	cmdsRan, _ := os.Create(cmdsRanPath)
	defer cmdsRan.Close()

	allOK := true

	for i, tmpl := range cmds {
		if ctx.Err() != nil {
			allOK = false
			break
		}
		if strings.TrimSpace(tmpl) == "" || strings.HasPrefix(strings.TrimSpace(tmpl), "#") {
			continue
		}
		stepIdx := i + 1
		stepLabel := stepName(tmpl)

		expanded := tmpl
		expanded = strings.ReplaceAll(expanded, "{STEPIDX}", fmt.Sprintf("%d", stepIdx))
		expanded = strings.ReplaceAll(expanded, "{STEP}", stepLabel)

		requireTarget := !*useStdin && !noTargetMode
		if requireTarget {
			if !strings.Contains(expanded, "{TARGET}") {
				io.WriteString(combErr, fmt.Sprintf("[!] Command missing {TARGET}: %s\n", tmpl))
				allOK = false
				continue
			}
			expanded = strings.ReplaceAll(expanded, "{TARGET}", escapeForShell(target))
		} else {
			// Not required, but if present, still substitute
			if strings.Contains(expanded, "{TARGET}") {
				expanded = strings.ReplaceAll(expanded, "{TARGET}", escapeForShell(target))
			}
		}

		// record expanded
		io.WriteString(cmdsRan, expanded+"\n")
		if saveExp != nil {
			saveExp.WriteString(expanded + "\n")
		}
		if *dryRun {
			fmt.Println(expanded)
			continue
		}

		// per-step files
		stepOut := filepath.Join(tdir, fmt.Sprintf("step-%d-%s.out", stepIdx, stepLabel))
		stepErr := filepath.Join(tdir, fmt.Sprintf("step-%d-%s.err", stepIdx, stepLabel))
		so, _ := os.Create(stepOut)
		se, _ := os.Create(stepErr)

		// tee?
		var outW io.Writer = so
		var errW io.Writer = se
		if *teeOutput {
			outW = io.MultiWriter(os.Stdout, so)
			errW = io.MultiWriter(os.Stderr, se)
		}

		// retries + backoff + qps
		minBO, maxBO := parseBackoff(*backoffRange)
		attempt := 0
		var rc int
		start := time.Now()

		for {
			attempt++
			if gate != nil {
				gate.Wait()
			}
			rc = runOne(ctx, expanded, target, outW, errW)
			if rc == 0 || attempt > *retries {
				break
			}
			time.Sleep(expBackoff(attempt, minBO, maxBO))
		}

		dur := time.Since(start)

		so.Close()
		se.Close()
		copyFileAppend(stepOut, combOut)
		copyFileAppend(stepErr, combErr)

		if aggOut != nil {
			aggMu.Lock()
			copyFileAppend(stepOut, aggOut)
			aggMu.Unlock()
		}
		if aggErr != nil {
			aggMu.Lock()
			copyFileAppend(stepErr, aggErr)
			aggMu.Unlock()
		}

		if rc != 0 {
			io.WriteString(combErr, fmt.Sprintf("[XX] %s | step %d '%s' rc=%d (%s)\n", target, stepIdx, stepLabel, rc, dur))
			allOK = false
			if incFail() {
				fmt.Fprintln(os.Stderr, "[!] Reached --stop-after-fail; aborting.")
				os.Exit(1)
			}
		} else {
			io.WriteString(combOut, fmt.Sprintf("[OK] %s | step %d '%s' (%s)\n", target, stepIdx, stepLabel, dur))
		}
	}

	return allOK
}

func runOne(parent context.Context, expanded string, target string, stdout, stderr io.Writer) int {
	// Choose shell
	var shell, flagC string
	if runtime.GOOS == "windows" && !isWSL() {
		shell, flagC = "cmd.exe", "/C"
	} else {
		if _, err := exec.LookPath("bash"); err == nil {
			shell, flagC = "bash", "-lc"
		} else {
			shell, flagC = "sh", "-lc"
		}
	}

	// timeout
	ctx := parent
	var cancel context.CancelFunc
	if *timeoutSec > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(*timeoutSec)*time.Second)
		defer cancel()
	}

	var cmd *exec.Cmd
	if *useStdin {
		cmd = exec.CommandContext(ctx, shell, flagC, expanded)
		cmd.Stdin = strings.NewReader(target + "\n")
	} else {
		cmd = exec.CommandContext(ctx, shell, flagC, expanded)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}

// ---------- helpers ----------

func loadLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		line := reCRLF.ReplaceAllString(sc.Text(), "")
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func filterTargets(in []string) []string {
	var out []string
	for _, l := range in {
		trim := strings.TrimSpace(reCRLF.ReplaceAllString(l, ""))
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		out = append(out, trim)
	}
	return out
}

func filterCmds(in []string) []string {
	var out []string
	for _, l := range in {
		trim := strings.TrimSpace(reCRLF.ReplaceAllString(l, ""))
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		out = append(out, trim)
	}
	return out
}

// remove duplicate targets while preserving order
func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func sanitize(s string) string { return reSan.ReplaceAllString(s, "_") }

func stepName(tmpl string) string {
	trim := strings.TrimSpace(tmpl)
	if trim == "" {
		return "step"
	}
	first := strings.Fields(trim)
	name := "step"
	if len(first) > 0 {
		name = first[0]
	}
	name = strings.ReplaceAll(name, "{TARGET}", "target")
	name = reSan.ReplaceAllString(name, "_")
	if name == "" {
		return "step"
	}
	return name
}

// shell-escape minimally for inline replacement
func escapeForShell(s string) string {
	if !strings.Contains(s, `'`) {
		return "'" + s + "'"
	}
	var b bytes.Buffer
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\'' {
			b.WriteString(`'\''`)
		} else {
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

func copyFileAppend(path string, w io.Writer) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = io.Copy(w, f)
}

func exitf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }

// ---------- resume ----------

func loadResume(path string) map[string]bool {
	m := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		if t != "" && !strings.HasPrefix(t, "#") {
			m[t] = true
		}
	}
	return m
}

// ---------- rate limit ----------

type rateGate struct {
	minGap time.Duration
	mu     sync.Mutex
	last   time.Time
}

func newRateGate(qps float64) *rateGate {
	if qps <= 0 {
		return nil
	}
	gap := time.Duration(float64(time.Second) / qps)
	return &rateGate{minGap: gap}
}

func (g *rateGate) Wait() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if !g.last.IsZero() {
		if d := now.Sub(g.last); d < g.minGap {
			time.Sleep(g.minGap - d)
		}
	}
	g.last = time.Now()
}

// ---------- backoff ----------

func parseBackoff(s string) (min, max time.Duration) {
	min, max = 1*time.Second, 60*time.Second
	parts := strings.SplitN(strings.TrimSpace(s), ",", 2)
	if len(parts) >= 1 {
		if n := atoiSafe(parts[0], 1); n > 0 {
			min = time.Duration(n) * time.Second
		}
	}
	if len(parts) == 2 {
		if n := atoiSafe(parts[1], 60); n > 0 {
			max = time.Duration(n) * time.Second
		}
	}
	if max < min {
		max = min
	}
	return
}

func expBackoff(attempt int, min, max time.Duration) time.Duration {
	d := min << (attempt - 1) // 1x, 2x, 4x, ...
	if d > max {
		d = max
	}
	// jitter in [0, d/2]
	j := time.Duration(rand.Int63n(int64(d/2 + 1)))
	return d/2 + j
}

func atoiSafe(s string, def int) int {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// crude WSL detect for shell choice
func isWSL() bool {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return bytes.Contains(bytes.ToLower(data), []byte("microsoft"))
}

func reached(ctx context.Context) bool { return ctx.Err() != nil }
