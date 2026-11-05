#!/usr/bin/env python3
"""Download Sub2API, apply all plugin patches, run tests, and build the installer."""

import argparse
import gzip
import json
import os
from pathlib import Path, PurePosixPath
import re
import runpy
import shutil
import subprocess
import sys
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
UPSTREAM = "https://github.com/Wei-Shaw/sub2api.git"
PNPM = "pnpm@12.16.1"
TARGETS = (("linux", "amd64"), ("linux", "arm64"), ("darwin", "amd64"), ("darwin", "arm64"), ("windows", "amd64"))
PTERM = "github.com/pterm/pterm@v0.12.84"


def resolve_upstream_tag():
    tag = os.environ.get("UPSTREAM_TAG", "").strip()
    if not tag:
        request = urllib.request.Request(
            "https://api.github.com/repos/Wei-Shaw/sub2api/releases/latest",
            headers={"Accept": "application/vnd.github+json", "User-Agent": "Sub2API-Plugins-build"},
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                release = json.load(response)
        except (OSError, ValueError) as error:
            raise RuntimeError(f"Cannot read the latest upstream release: {error}") from error
        tag = release.get("tag_name") if isinstance(release, dict) else None
    if not isinstance(tag, str) or not re.fullmatch(r"v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?", tag):
        raise RuntimeError(f"Invalid upstream version tag: {tag!r}")
    return tag


def artifact_name(system, arch, tag, installer=False):
    kind = "-installer" if installer else ""
    suffix = ".exe" if system == "windows" else ""
    return f"sub2api-plugins{kind}-{tag}-{system}-{arch}{suffix}"


def run(argv, cwd, env=None):
    print("+ " + " ".join(map(str, argv)), flush=True)
    subprocess.run(list(map(str, argv)), cwd=cwd, env=env, check=True)


def discover_plugins(root):
    if (root / "patches").is_dir():
        return [root]
    modules = root / ".gitmodules"
    if not modules.is_file():
        return []
    result = subprocess.run(
        ["git", "config", "--null", "--file", str(modules), "--get-regexp", r"^submodule\..*\.path$"],
        cwd=root, text=True, capture_output=True,
    )
    if result.returncode not in (0, 1):
        raise RuntimeError(result.stderr.strip())
    paths = [entry.split("\n", 1)[1] for entry in result.stdout.split("\0") if entry]
    plugins = sorted(path for path in paths if PurePosixPath(path).parts[0] == "plugins")
    for path in plugins:
        if not (root / path / ".git").exists():
            raise RuntimeError(f"Uninitialized submodule {path}. Run: git submodule update --init --recursive -- {path}")
    return [root / path for path in plugins if any((root / path / "patches").glob("*.patch"))]


def apply_patches(tree, plugins):
    for plugin in plugins:
        for patch in sorted((plugin / "patches").glob("*.patch")):
            try:
                run(["git", "apply", "--binary", str(patch)], tree)
            except subprocess.CalledProcessError as error:
                raise RuntimeError(f"Patch failed: {plugin.name}/{patch.name}") from error


def test_entries(owner, tree, env, installer=None, common=None):
    support = runpy.run_path(str(ROOT / "tests/support.py"))
    support["test_entries"](owner, tree, env, run, installer, common)


def collect_licenses(staging, root, plugins):
    destination = staging / "licenses"
    destination.mkdir()
    for owner in [root, *(plugin for plugin in plugins if plugin != root)]:
        namespace = Path() if owner == root else owner.relative_to(root / "plugins")
        target = destination / namespace
        target.mkdir(parents=True, exist_ok=True)
        if (owner / "LICENSE").is_file():
            shutil.copyfile(owner / "LICENSE", target / "LICENSE")
        if (owner / "licenses").is_dir():
            shutil.copytree(owner / "licenses", target, dirs_exist_ok=True)


def prepare_installer(workspace, tree, root, plugins, env):
    staging = workspace / "installer"
    shutil.copytree(root / "scripts/installer", staging)
    lines = (tree / "backend/go.mod").read_text().splitlines()
    directives = [line for line in lines if line.startswith(("go ", "toolchain "))]
    (staging / "go.mod").write_text("module sub2api-plugins-installer\n\n" + "\n".join(directives) + "\n")
    dependency_env = dict(env, GOFLAGS="-mod=mod")
    run(["go", "get", PTERM], staging, dependency_env)
    run(["go", "mod", "tidy"], staging, dependency_env)
    collect_licenses(staging, root, plugins)
    modules = subprocess.run(["go", "list", "-m", "-json", "all"], cwd=staging, env=env, text=True, capture_output=True, check=True)
    decoder = json.JSONDecoder()
    remaining = modules.stdout.lstrip()
    while remaining:
        module, end = decoder.raw_decode(remaining)
        remaining = remaining[end:].lstrip()
        if module.get("Main") or "Dir" not in module:
            continue
        directory = Path(module["Dir"])
        notices = [path for path in directory.iterdir() if path.is_file() and path.name.upper().startswith(("LICENSE", "COPYING", "NOTICE"))]
        for notice in notices:
            destination = staging / "licenses/installer-dependencies" / module["Path"] / notice.name
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(notice, destination)
    return staging


def publish_outputs(prepared, output):
    output.parent.mkdir(parents=True, exist_ok=True)
    publishing = Path(tempfile.mkdtemp(prefix=".sub2api-plugins-", dir=output.parent))
    candidate, previous = publishing / "candidate", publishing / "previous"
    try:
        # Complete any cross-filesystem copy before moving the existing output directory.
        shutil.copytree(prepared, candidate)
        if output.exists():
            output.rename(previous)
        try:
            candidate.rename(output)
        except OSError:
            if previous.exists():
                try:
                    previous.rename(output)
                except OSError as error:
                    raise RuntimeError(f"Restore previous outputs from {previous}: {error}") from error
            raise
    finally:
        if not previous.exists() or output.exists():
            shutil.rmtree(publishing)


def build(root, workspace, tag):
    version = tag.removeprefix("v")
    plugins = discover_plugins(root)
    tree = workspace / "source"
    run(["git", "clone", "--depth", "1", "--branch", tag, UPSTREAM, tree], workspace)
    run(["git", "checkout", "--detach", f"refs/tags/{tag}"], tree)
    apply_patches(tree, plugins)
    frontend = tree / "frontend"
    settings = json.loads((frontend / "package.json").read_text()).get("pnpm", {})
    # pnpm 12 moved overrides and build settings out of package.json and .npmrc.
    # Keep upstream pnpm 9's dependency-script behavior without changing its lockfile.
    settings.update(ignoreScripts=False, dangerouslyAllowAllBuilds=True, strictPeerDependencies=False)
    (frontend / "pnpm-workspace.yaml").write_text(json.dumps(settings, indent=2) + "\n")
    env = os.environ.copy()
    for name in ("GOOS", "GOARCH", "CGO_ENABLED"):
        env.pop(name, None)
    env.update(GOFLAGS="-mod=readonly", PYTHONDONTWRITEBYTECODE="1", SUB2API_BUILD_VERSION=version, SUB2API_PNPM=PNPM)
    env.setdefault("COREPACK_ENABLE_DOWNLOAD_PROMPT", "0")
    env.setdefault("COREPACK_ENABLE_AUTO_PIN", "0")
    env["COREPACK_ENABLE_PROJECT_SPEC"] = "0"
    run(["corepack", "prepare", PNPM, "--activate"], root, env)
    run(["corepack", PNPM, "install", "--frozen-lockfile"], tree / "frontend", env)
    run(["corepack", PNPM, "build"], tree / "frontend", env)
    for plugin in plugins:
        test_entries(plugin, tree, env, common=False)
    run(["go", "vet", "./internal/server", "./internal/server/routes", "./internal/web"], tree / "backend", env)
    staging = prepare_installer(workspace, tree, root, plugins, env)
    binaries = workspace / "binaries"
    binaries.mkdir()
    for system, arch in TARGETS:
        target = dict(env, GOOS=system, GOARCH=arch, CGO_ENABLED="0")
        binary = binaries / f"{system}-{arch}"
        run(["go", "build", "-tags=embed", "-trimpath", f"-ldflags=-s -w -X main.Version={version} -X main.BuildType=release", "-o", binary, "./cmd/server"], tree / "backend", target)
    payloads = staging / "payloads"
    payloads.mkdir()
    for binary in binaries.iterdir():
        with binary.open("rb") as original, gzip.open(payloads / (binary.name + ".gz"), "wb") as compressed:
            shutil.copyfileobj(original, compressed)
    test_entries(root, tree, env, staging, common=True if root in plugins else None)
    prepared = workspace / "outputs"
    prepared.mkdir()
    for system, arch in TARGETS:
        platform = f"{system}-{arch}"
        directory = prepared / platform
        directory.mkdir()
        binary = directory / artifact_name(system, arch, tag)
        shutil.copyfile(binaries / platform, binary)
        binary.chmod(0o755)
        selected = {platform, f"linux-{arch}"}
        for payload in payloads.iterdir():
            if payload.name.removesuffix(".gz") not in selected:
                payload.unlink()
        for name in selected:
            if not (payloads / (name + ".gz")).exists():
                with (binaries / name).open("rb") as original, gzip.open(payloads / (name + ".gz"), "wb") as compressed:
                    shutil.copyfileobj(original, compressed)
        target = dict(env, GOOS=system, GOARCH=arch, CGO_ENABLED="0")
        run(["go", "build", "-trimpath", f"-ldflags=-s -w -X main.buildVersion={version}", "-o", directory / artifact_name(system, arch, tag, installer=True), "."], staging, target)
    output = root / "dist/build"
    publish_outputs(prepared, output)
    return sorted(path for path in output.glob("*/*") if path.is_file())


def main():
    argparse.ArgumentParser(description=__doc__).parse_args()
    try:
        tag = resolve_upstream_tag()
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as output:
                print(f"tag={tag}", file=output)
        with tempfile.TemporaryDirectory(prefix="sub2api-plugins-") as temporary:
            print(f"Build workspace: {temporary}", flush=True)
            for output in build(ROOT, Path(temporary), tag):
                print(output, flush=True)
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"Build failed: {error}. No new outputs were published.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
