#!/usr/bin/env python3
"""Does codewiki help a coding agent? A small, repeatable eval.

Questions about the Go standard library are generated from the go/types
ground truth written by scripts/accuracy (never from codewiki's own
answers), so every answer can be scored as a set:

  callers     "Which functions and methods call X directly?"
  implements  "Which types in package P implement interface I?"

Each question is put to Claude Code (`claude -p`) twice: with only its
built-in read and search tools, and with those plus codewiki's MCP tools.
Every run records the answer's precision, recall and F1 against the truth,
and the tokens, turns, time and cost it took.

Running it spends the account's Claude usage; --dry-run prints the
questions and expected answers without calling Claude.

Usage:
  python3 run.py --truth DIR --index STD.db --codewiki BIN --dry-run
  python3 run.py --truth DIR --index STD.db --codewiki BIN --questions 6 --repeats 2 --out results.jsonl
  python3 run.py --report results.jsonl
"""
import argparse
import json
import os
import random
import re
import statistics
import subprocess
import sys
import tempfile
import time

FORMAT = ("End your reply with a line containing only ANSWER:, then one name per line, "
          "written as import/path.Name or import/path.Type.Method (for example "
          "net/http.Server.Serve or io/fs.ReadFile), and nothing after the list.")


def lines(path):
    with open(path) as f:
        return [l for l in f.read().splitlines() if l]


def questions(truth_dir, n, seed):
    """Generates n questions, half of each kind, with 3 to 12 true answers."""
    rng = random.Random(seed)
    callers = {}
    for l in lines(os.path.join(truth_dir, "calls.txt")):
        a, b = l.split()
        callers.setdefault(b, set()).add(a)
    impls = {}
    for l in lines(os.path.join(truth_dir, "implements.txt")):
        t, i = l.split()
        pkg = t.rsplit(".", 1)[0]
        impls.setdefault((i, pkg), set()).add(t)

    def exported(name):
        return name.rsplit(".", 1)[-1][:1].isupper()

    call_pool = sorted(k for k, v in callers.items()
                       if 3 <= len(v) <= 12 and exported(k) and not k.startswith(("internal/", "cmd/", "runtime")))
    impl_pool = sorted(k for k, v in impls.items()
                       if 3 <= len(v) <= 12 and exported(k[0]) and not k[1].startswith(("internal/", "cmd/")))
    out = []
    for callee in rng.sample(call_pool, min(n - n // 2, len(call_pool))):
        out.append({
            "id": "callers:" + callee,
            "prompt": f"In the Go standard library source in the current directory, which functions and methods "
                      f"call {callee} directly (not through other functions)? Include unexported ones. " + FORMAT,
            "expected": sorted(callers[callee]),
        })
    for iface, pkg in rng.sample(impl_pool, min(n // 2, len(impl_pool))):
        out.append({
            "id": f"implements:{iface}@{pkg}",
            "prompt": f"In the Go standard library source in the current directory, which named types declared in "
                      f"package {pkg} implement the interface {iface} (as a value or through a pointer)? "
                      f"Include unexported types. " + FORMAT,
            "expected": sorted(impls[(iface, pkg)]),
        })
    return out


def parse_answer(text):
    """Returns the names listed after the last ANSWER: line, normalized."""
    if "ANSWER:" not in text:
        return []
    names = []
    for l in text.rsplit("ANSWER:", 1)[1].splitlines():
        l = l.strip().strip("-*`• ").strip()
        l = re.sub(r"[()*]", "", l)  # (*T).M -> T.M
        l = re.sub(r"\s+", "", l)
        if l:
            names.append(l)
    return names


def score(answer, expected):
    got, want = set(answer), set(expected)
    hit = len(got & want)
    p = hit / len(got) if got else 0.0
    r = hit / len(want) if want else 0.0
    f1 = 2 * p * r / (p + r) if p + r else 0.0
    return {"precision": p, "recall": r, "f1": f1}


def run_claude(q, src, condition, binary, index, model, budget):
    tools = ["Read", "Grep", "Glob"]
    cmd = ["claude", "-p", q["prompt"], "--output-format", "json", "--strict-mcp-config",
           "--max-budget-usd", str(budget), "--permission-mode", "dontAsk"]
    if model:
        cmd += ["--model", model]
    cfg = None
    if condition == "codewiki":
        cfg = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
        json.dump({"mcpServers": {"codewiki": {"command": binary,
                                               "args": ["serve", "--db", index, "--watch=false"]}}}, cfg)
        cfg.close()
        cmd += ["--mcp-config", cfg.name]
        tools.append("mcp__codewiki")
    cmd += ["--allowedTools", ",".join(tools)]
    start = time.time()
    try:
        proc = subprocess.run(cmd, cwd=src, capture_output=True, text=True, timeout=900)
    finally:
        if cfg:
            os.unlink(cfg.name)
    elapsed = time.time() - start
    try:
        res = json.loads(proc.stdout)
    except json.JSONDecodeError:
        return {"error": (proc.stderr or proc.stdout)[-500:], "seconds": elapsed}
    usage = res.get("usage", {})
    answer = parse_answer(res.get("result", ""))
    return {
        "answer": answer,
        **score(answer, q["expected"]),
        "turns": res.get("num_turns"),
        "cost_usd": res.get("total_cost_usd"),
        "input_tokens": usage.get("input_tokens", 0) + usage.get("cache_read_input_tokens", 0)
        + usage.get("cache_creation_input_tokens", 0),
        "output_tokens": usage.get("output_tokens", 0),
        "seconds": elapsed,
        "is_error": res.get("is_error", False),
    }


def report(path):
    rows = [json.loads(l) for l in open(path) if l.strip()]
    by = {}
    for r in rows:
        by.setdefault(r["condition"], []).append(r)
    print(f"{len(rows)} runs\n")
    print("| Condition | Runs | Errors | Mean F1 | Mean recall | Mean precision | Median input tokens | Median turns | Median seconds | Total cost |")
    print("|---|---|---|---|---|---|---|---|---|---|")
    for cond in ("baseline", "codewiki"):
        rs = by.get(cond, [])
        ok = [r for r in rs if "error" not in r]
        if not ok:
            continue
        mean = lambda k: statistics.mean(r[k] for r in ok)
        med = lambda k: statistics.median(r[k] for r in ok)
        cost = sum(r["cost_usd"] or 0 for r in ok)
        print(f"| {cond} | {len(rs)} | {len(rs) - len(ok)} | {mean('f1'):.2f} | {mean('recall'):.2f} | "
              f"{mean('precision'):.2f} | {med('input_tokens'):,.0f} | {med('turns'):.0f} | {med('seconds'):.0f} | ${cost:.2f} |")


def self_test():
    assert parse_answer("blah\nANSWER:\n- net/http.Server.Serve\n* `(*io.Writer).Write`\n\n") == \
        ["net/http.Server.Serve", "io.Writer.Write"]
    assert parse_answer("no marker") == []
    s = score(["a", "b", "x"], ["a", "b", "c", "d"])
    assert abs(s["precision"] - 2 / 3) < 1e-9 and abs(s["recall"] - 0.5) < 1e-9
    print("self-test ok")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--truth", help="directory written by scripts/accuracy (calls.txt, implements.txt)")
    ap.add_argument("--index", help="codewiki index of the standard library")
    ap.add_argument("--codewiki", help="codewiki binary")
    ap.add_argument("--src", default=None, help="standard library source (default: $(go env GOROOT)/src)")
    ap.add_argument("--questions", type=int, default=6)
    ap.add_argument("--repeats", type=int, default=2)
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--model", default=None)
    ap.add_argument("--budget", type=float, default=1.0, help="max USD per single run")
    ap.add_argument("--out", default="results.jsonl")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--report", metavar="RESULTS")
    ap.add_argument("--self-test", action="store_true")
    a = ap.parse_args()
    if a.self_test:
        return self_test()
    if a.report:
        return report(a.report)
    if not a.truth:
        sys.exit("--truth is required")
    qs = questions(a.truth, a.questions, a.seed)
    if a.dry_run:
        for q in qs:
            print(f"{q['id']}  ({len(q['expected'])} expected)\n  {q['prompt']}\n  expected: {', '.join(q['expected'])}\n")
        return
    if not (a.index and a.codewiki):
        sys.exit("--index and --codewiki are required")
    src = a.src or os.path.join(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip(), "src")
    with open(a.out, "a") as out:
        for rep in range(a.repeats):
            for q in qs:
                for cond in ("baseline", "codewiki"):
                    r = run_claude(q, src, cond, os.path.abspath(a.codewiki), os.path.abspath(a.index), a.model, a.budget)
                    row = {"question": q["id"], "condition": cond, "repeat": rep, "expected": q["expected"], **r}
                    out.write(json.dumps(row) + "\n")
                    out.flush()
                    print(f"{q['id']} {cond} #{rep}: F1 {r.get('f1', 0):.2f}, {r.get('input_tokens', 0):,} input tokens"
                          + (f" ERROR {r['error'][:120]}" if "error" in r else ""), file=sys.stderr)
    report(a.out)


if __name__ == "__main__":
    main()
