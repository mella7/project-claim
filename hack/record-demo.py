#!/usr/bin/env python3
"""Records the demo in docs/demo.gif.

Runs real kubectl commands against the cluster in KUBECONFIG, with the
operator running, and writes an asciicast file. Command output and the time
each command took are real. Only the typing is simulated. Render with agg:

    make sandbox                 # terminal 1
    make run-sandbox             # terminal 2
    KUBECONFIG=bin/sandbox.kubeconfig python3 hack/record-demo.py demo.cast
    agg --font-size 15 demo.cast docs/demo.gif
"""
import json
import subprocess
import sys
import time

COLS, ROWS = 96, 22
GREEN, DIM, BOLD, RESET = "\x1b[32m", "\x1b[90m", "\x1b[1m", "\x1b[0m"
PROMPT = f"{GREEN}${RESET} "

events = []
clock = 0.0


def out(text):
    events.append([round(clock, 3), "o", text.replace("\n", "\r\n")])


def wait(seconds):
    global clock
    clock += seconds


def note(text):
    out(f"{DIM}# {text}{RESET}\n")
    wait(1.4)


def run(command, pause=2.2, settle=0.0):
    """Type a command, run it for real, show what it printed."""
    out(PROMPT)
    wait(0.4)
    for ch in command:
        out(ch)
        wait(0.028)
    wait(0.35)
    out("\n")
    started = time.monotonic()
    result = subprocess.run(command, shell=True, capture_output=True, text=True)
    wait(time.monotonic() - started)
    out(result.stdout + result.stderr)
    wait(pause)
    if settle:
        time.sleep(settle)  # give the operator time to react, off camera


def scene(title):
    out("\x1b[2J\x1b[H")
    out(f"{BOLD}{title}{RESET}\n\n")
    wait(1.0)


def quiet(command):
    subprocess.run(command, shell=True, capture_output=True)


def main():
    target = sys.argv[1] if len(sys.argv) > 1 else "demo.cast"
    quiet("kubectl apply -f docs/demo/platform.yaml")
    time.sleep(1)

    scene("1. One claim becomes a governed namespace")
    run("cat docs/demo/staging.yaml", pause=3.0)
    run("kubectl apply -f docs/demo/staging.yaml", pause=1.0, settle=2.0)
    scene("1. One claim becomes a governed namespace")
    run("kubectl get projectclaims", pause=2.5)
    run("kubectl get quota,limits,rolebindings,netpol -n payments-staging -o name", pause=3.5)

    scene("2. Manual changes are reverted")
    run("kubectl delete quota projectclaim-quota -n payments-staging", pause=0.8, settle=1.5)
    note("gone? the operator has already put it back")
    run("kubectl get quota -n payments-staging", pause=3.5)

    scene("3. The team's budget is 6 CPU. A second project would make it 8.")
    run("kubectl apply -f docs/demo/prod.yaml", pause=1.0, settle=2.0)
    run("kubectl get projectclaims", pause=2.5)
    run("kubectl get projectclaim payments-prod -o jsonpath='{.status.conditions[0].message}'; echo", pause=3.5)
    note("raise the budget: the waiting claim is admitted on its own")
    run("kubectl patch tenant payments --type merge -p '{\"spec\":{\"quotaBudget\":{\"requests.cpu\":\"8\"}}}'",
        pause=1.0, settle=2.0)
    run("kubectl get projectclaims", pause=2.0)
    run("kubectl get tenants", pause=5.0)

    with open(target, "w") as f:
        f.write(json.dumps({"version": 2, "width": COLS, "height": ROWS}) + "\n")
        for event in events:
            f.write(json.dumps(event) + "\n")
    print(f"wrote {target}: {clock:.0f}s, {len(events)} events")


if __name__ == "__main__":
    main()
