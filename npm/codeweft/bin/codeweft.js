#!/usr/bin/env node
// Runs the codeweft binary for this platform. npm installs it from the one
// @codeweft/<os>-<cpu> package (an optional dependency) matching the
// platform; this script only finds it and hands over the terminal,
// arguments, standard input and output, so MCP clients talk to the binary
// directly.
"use strict";

const { spawnSync } = require("child_process");
const path = require("path");

function binary() {
  if (process.env.CODEWEFT_BINARY) return process.env.CODEWEFT_BINARY;
  const pkg = `@codeweft/${process.platform}-${process.arch}`;
  const exe = process.platform === "win32" ? "codeweft.exe" : "codeweft";
  try {
    return path.join(path.dirname(require.resolve(`${pkg}/package.json`)), "bin", exe);
  } catch (e) {
    console.error(
      `codeweft: no prebuilt binary for ${process.platform}/${process.arch} (package ${pkg} is not installed).\n` +
        "Builds exist for Linux (x64, arm64), macOS (arm64, x64) and Windows (x64). If you installed with\n" +
        "--no-optional or --omit=optional, reinstall without it. Otherwise download a release from\n" +
        "https://github.com/anjanikumarjangid/codeweft/releases and set CODEWEFT_BINARY to its path."
    );
    process.exit(1);
  }
}

const result = spawnSync(binary(), process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  console.error(`codeweft: ${result.error.message}`);
  process.exit(1);
}
if (result.signal) {
  process.kill(process.pid, result.signal);
} else {
  process.exit(result.status === null ? 1 : result.status);
}
