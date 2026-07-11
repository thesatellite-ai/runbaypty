# 07. Per-command exit codes and output (the Warp "blocks" trick)

> With OSC 133 shell-integration marks, the daemon tracks each command's boundaries and exit code.

**What it shows:** structured command results (which command, its exit code, its exact output range) instead of screen-scraping. This is what powers Warp's "blocks".

## Setup

```sh
export RUNBAYPTY_HOME=$(mktemp -d); export RUNBAYPTY_SOCK=/tmp/rpty-play.sock
bin/runbaypty serve &
```

## Try it by hand

### Step 1: spawn a session that emits OSC 133 marks

Real shells emit these automatically with shell integration; here we emit them by hand around two commands, one that exits 0 and one that exits 3. The leading `sleep 1` gives us time to subscribe before the marks fire.

```sh
ID=$(bin/runbaypty run --name work -- sh -c '
  sleep 1
  printf "\033]133;C\007"; echo "step 1 ok";     printf "\033]133;D;0\007"; sleep .3
  printf "\033]133;C\007"; echo "step 2 FAILED"; printf "\033]133;D;3\007"; sleep 3')
```

`ESC ] 133 ; C BEL` marks the start of a command's output; `ESC ] 133 ; D ; <code> BEL` marks the end with the exit code.

### Step 2: watch the command-finished events

```sh
bin/runbaypty events --json --session "$ID" | grep -m2 command-finished
```

Two events, each with the exit code and the byte range of that command's output:

```
{"Type":"command-finished","SessionID":"ses_...","Data":{"end_seq":"36","exit_code":"0","start_seq":"8"}}
{"Type":"command-finished","SessionID":"ses_...","Data":{"end_seq":"76","exit_code":"3","start_seq":"44"}}
```

### Step 3: pull the last command's exact output

```sh
bin/runbaypty lastcmd work
```

```
step 2 FAILED
```

`lastcmd` returns the output window of the most recently finished command, sliced out by its recorded boundaries. No parsing, no guessing where it began.

## What just happened

The daemon turned in-band shell marks into structured events: for each command it knows where the output started (`start_seq`), where it ended (`end_seq`), and the exit code. An agent can ask "did the last command succeed, and show me only its output" without ever screen-scraping the terminal. See the [command-boundaries](../command-boundaries/) example for the SDK version.

## Run it all at once

```sh
bash examples/snippets/07-per-command-blocks.sh
```
