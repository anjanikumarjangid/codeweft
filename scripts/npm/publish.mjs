#!/usr/bin/env node
// Builds the npm packages of a release from its archives and publishes them:
// one @codeweft/<os>-<cpu> package per platform with its binary, then the
// codeweft package that depends on them all, optionally, and runs the
// one npm installs for the platform.
//
//   node scripts/npm/publish.mjs --dist DIR --version 0.8.2 [--skip os-cpu,...] [--dry-run]
//
// DIR holds the release archives (codeweft_<version>_<goos>_<goarch>.tar.gz
// or .zip). With --dry-run the packages are packed, not published.
import { execFileSync } from "node:child_process";
import { chmodSync, copyFileSync, cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const args = process.argv.slice(2);
const opt = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
};
const dist = resolve(opt("--dist") ?? "dist");
const version = (opt("--version") ?? "").replace(/^v/, "");
const dryRun = args.includes("--dry-run");
// --skip leaves platforms out, e.g. "win32-x64": their package is neither
// published nor listed in codeweft's optionalDependencies, so codeweft can
// be released while npm refuses one platform's package.
const skip = new Set((opt("--skip") ?? "").split(",").map((s) => s.trim()).filter(Boolean));
if (!/^\d+\.\d+\.\d+(-[\w.]+)?$/.test(version)) {
  console.error("usage: publish.mjs --dist DIR --version X.Y.Z [--skip os-cpu,...] [--dry-run]");
  process.exit(2);
}

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
// Go's names for platforms, and npm's.
const allPlatforms = [
  { goos: "linux", goarch: "amd64", os: "linux", cpu: "x64" },
  { goos: "linux", goarch: "arm64", os: "linux", cpu: "arm64" },
  { goos: "darwin", goarch: "arm64", os: "darwin", cpu: "arm64" },
  { goos: "darwin", goarch: "amd64", os: "darwin", cpu: "x64" },
  { goos: "windows", goarch: "amd64", os: "win32", cpu: "x64" },
];
for (const name of skip) {
  if (!allPlatforms.some((p) => `${p.os}-${p.cpu}` === name)) {
    console.error(`--skip: unknown platform ${name}`);
    process.exit(2);
  }
}
const platforms = allPlatforms.filter((p) => !skip.has(`${p.os}-${p.cpu}`));
if (skip.size) console.log(`leaving out: ${[...skip].join(", ")}`);
const work = mkdtempSync(join(tmpdir(), "codeweft-npm-"));
const run = (cmd, argv, cwd) => execFileSync(cmd, argv, { cwd, stdio: "inherit" });
// published reports whether this version of a package is already on the
// registry, so a re-run after a partial publish carries on from there.
const published = (name) => {
  try {
    const out = execFileSync("npm", ["view", `${name}@${version}`, "version"], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"], // a 404 is the expected answer
    });
    return out.trim() === version;
  } catch {
    return false; // not found
  }
};
// publish publishes a package, trying again on failure: provenance is
// signed into a public transparency log, and a lost reply there makes npm
// fail with a 409 for an entry that its own retry duplicated.
const publish = (dir) => {
  if (dryRun) return run("npm", ["pack", "--dry-run"], dir);
  const { name } = JSON.parse(readFileSync(join(dir, "package.json"), "utf8"));
  for (let attempt = 1; ; attempt++) {
    if (published(name)) {
      console.log(`${name}@${version} is published`);
      return;
    }
    try {
      return run("npm", ["publish", "--access", "public", "--provenance"], dir);
    } catch (err) {
      if (attempt === 3) throw err;
      console.error(`publishing ${name} failed (attempt ${attempt} of 3); trying again in ${10 * attempt} s`);
      execFileSync("sleep", [String(10 * attempt)]);
    }
  }
};
const base = JSON.parse(readFileSync(join(root, "npm/codeweft/package.json"), "utf8"));

const deps = {};
for (const p of platforms) {
  const stem = `codeweft_${version}_${p.goos}_${p.goarch}`;
  const archive = [".tar.gz", ".zip"].map((ext) => join(dist, stem + ext)).find(existsSync);
  if (!archive) {
    console.error(`missing ${stem}.tar.gz or .zip in ${dist}`);
    process.exit(1);
  }
  const unpacked = join(work, stem);
  mkdirSync(unpacked, { recursive: true });
  if (archive.endsWith(".zip")) run("unzip", ["-q", archive, "-d", unpacked]);
  else run("tar", ["-xzf", archive, "-C", unpacked]);
  const exe = p.goos === "windows" ? "codeweft.exe" : "codeweft";
  // Scoped: only this project can publish under @codeweft, and npm's
  // spam check refused the unscoped codeweft-win32-x64.
  const name = `@codeweft/${p.os}-${p.cpu}`;
  const pkg = join(work, name);
  mkdirSync(join(pkg, "bin"), { recursive: true });
  copyFileSync(join(unpacked, stem, exe), join(pkg, "bin", exe));
  chmodSync(join(pkg, "bin", exe), 0o755);
  for (const f of ["LICENSE", "NOTICE"]) copyFileSync(join(root, f), join(pkg, f));
  writeFileSync(
    join(pkg, "package.json"),
    JSON.stringify(
      {
        name,
        version,
        description: `The codeweft binary for ${p.os} ${p.cpu}. Install codeweft instead.`,
        license: base.license,
        repository: base.repository,
        os: [p.os],
        cpu: [p.cpu],
        files: ["bin", "LICENSE", "NOTICE"],
        preferUnplugged: true,
      },
      null,
      2
    ) + "\n"
  );
  publish(pkg);
  deps[name] = version;
}

const main = join(work, "codeweft");
cpSync(join(root, "npm/codeweft"), main, { recursive: true });
for (const f of ["LICENSE", "NOTICE"]) copyFileSync(join(root, f), join(main, f));
writeFileSync(
  join(main, "package.json"),
  JSON.stringify({ ...base, version, files: [...base.files, "LICENSE", "NOTICE"], optionalDependencies: deps }, null, 2) + "\n"
);
// Refuse to publish a package that would install without its launcher:
// 0.8.1 went out that way, its bin/ directory missing from the repository.
const pkgJSON = JSON.parse(readFileSync(join(main, "package.json"), "utf8"));
for (const f of [...Object.values(pkgJSON.bin ?? {}), ...pkgJSON.files]) {
  if (!existsSync(join(main, f))) {
    console.error(`codeweft package: ${f} is missing from npm/codeweft`);
    process.exit(1);
  }
}
publish(main);
console.log(`${dryRun ? "packed" : "published"} codeweft ${version} for ${platforms.length} platforms`);
