# runlist

Tiny, fast Go runner for “one-line-per-target” workflows.

- Parallel **per-target** execution, steps run sequentially within a target
- Commands from `--cmd-file` or repeated `-c` flags
- Placeholders: `{TARGET}`, `{STEP}`, `{STEPIDX}`
- **Stdin mode** for tools like `dnsx`, `httpx`, etc.
- Reliability features: `--retries`, `--backoff`, `--stop-after-fail`, `--resume`
- **Live logging** to console and files: `--tee`
- **Rate limiting** of step *starts*: `--qps`
- Flexible inputs: `-i FILE`, `--target VALUE`, **piped STDIN**, or **no targets** (single-run mode)
- Output per target: `step-*.out/.err`, `combined.out/.err`, `commands.ran`

---

## Table of Contents

- [Install / Build](#install--build)
- [Command File Format](#command-file-format)
- [Ways to Provide Targets](#ways-to-provide-targets)
- [Stdin Mode (dnsx/httpx/etc.)](#stdin-mode-dnsxhttpxetc)
- [Examples](#examples)
- [Flags](#flags)
- [Outputs](#outputs)
- [Troubleshooting](#troubleshooting)
- [License](#license)

---

## Install / Build

```bash
go build -o runlist runlist.go
./runlist -h
```
Command File Format

One command per line (no \ line breaks).

Blank lines and lines starting with # are ignored.

Placeholders:

{TARGET} → replaced with the current target (unless --stdin is used)

{STEP} → first token of the command (sanitized), used in filenames

{STEPIDX} → 1-based step index

Example cmds.txt:

```
echo hello {TARGET}
amass enum -passive -d {TARGET} | sort -u
```

Ways to Provide Targets

Pick one of these input modes:

A) From a file
```
./runlist -i targets.txt --cmd-file cmds.txt -P 20 -o out
```
B) Single target
```
./runlist --target example.com --cmd-file cmds.txt -P 1 -o out
```
C) Piped via STDIN
```
cat targets.txt | ./runlist --cmd-file cmds.txt -P 20 -o out
subfinder -d example.com | ./runlist --cmd-file cmds.txt -P 20 -o out
```
D) Only --cmd-file (no targets at all)

Runs once with a synthetic target __run__ (useful for global tasks or stdin-only steps).
```
./runlist --cmd-file cmds.txt -o out
# outputs: out/__run__/...
```

Examples

Hello test (sanity check):
```
printf "example.com\n" > one.txt
printf "echo hello {TARGET}\n" > cmds.test
./runlist -i one.txt --cmd-file cmds.test -P 1 --keep-order --tee -o outtest
cat outtest/example.com/combined.out

```

Careful API scraping:

```
./runlist -i targets.txt --cmd-file api_cmds.txt \
  -P 20 --qps 5 --retries 2 --backoff "2,20" --tee -o out

```

Large scope with resume:

```
./runlist -i scope.txt --cmd-file cmds.txt \
  -P 24 --resume resume.state --unique --shuffle -o out
# Re-run later; completed targets are skipped.

```

Fail fast if something’s wrong:
```
./runlist -i targets.txt --cmd-file cmds.txt \
  -P 24 --stop-after-fail 25 --tee -o out

```


Flags
Core

-i FILE — targets list (one per line)

--target VALUE — single target (no -i)

--cmd-file FILE — commands file (one per line)

-c "CMD" — add a command inline (repeatable)

-o DIR — output directory (default: runlist_out)

-P N — parallel targets (steps are sequential per target)

-t SECONDS — per-step timeout

Input modes

--stdin — pipe the target into each step’s STDIN (omit {TARGET} in commands)

Without -i/--target, if STDIN is piped, targets are read from STDIN.

Without -i/--target/STDIN, runs once with synthetic target __run__.

Reliability / Speed

--tee — mirror stdout/stderr to console and write to files

--retries N — retry failing steps up to N times

--backoff "min,max" — seconds for exponential backoff with jitter (default "1,60")

--qps N — rate limit step starts across all workers

--stop-after-fail N — abort whole job after N step failures (global)

--resume FILE — skip targets that completed successfully in a prior run

--unique — de-duplicate target list

--shuffle — randomize target order

Auditing

--dry-run — print expanded commands and exit (no execution)

--save-expanded FILE — append each expanded command to a file

--aggregate FILE — append all stdout to a single file

--err-aggregate FILE — append all stderr to a single file

--keep-order — process targets serially (useful for debugging)

Outputs

Per target (or __run__ for single-run mode):

```
out/<target>/
  commands.ran         # every expanded command that ran
  step-1-<name>.out    # stdout of step 1
  step-1-<name>.err    # stderr of step 1
  step-2-<name>.out
  step-2-<name>.err
  combined.out         # concatenated stdout + [OK] lines
  combined.err         # concatenated stderr + [XX] lines / warnings

```

<name> is derived from the first token of the command (sanitized), or from {STEP}.

Troubleshooting

“Command missing {TARGET}”
You’re not using --stdin, and the command line lacks {TARGET}.
→ Either add {TARGET}, or run with --stdin (if the tool reads stdin).

No targets but commands expect {TARGET}
Running only --cmd-file enters single-run mode (__run__).
→ Add --target, or supply -i/STDIN, or remove {TARGET} and use --stdin if possible.

Nothing seems to run
Check out/*/commands.ran to see what was attempted, and combined.err for errors.

Windows/WSL
The runner prefers bash if available, otherwise falls back to sh or cmd.exe on native Windows.
