#!/usr/bin/env python3
"""Download Sub2API, apply all plugin patches, run tests, and build the installer."""

import argparse
import gzip
import json
import os
import platform
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


def selected_platforms(value=None):
    supported = [f"{system}-{arch}" for system, arch in TARGETS]
    if value is None:
        return supported
    selected = value.split()
    if not selected or any(target not in supported for target in selected):
        raise RuntimeError("Select at least one supported build platform.")
    return [target for target in supported if target in selected]


def common_targets():
    arch = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[platform.machine().lower()]
    return sorted({"linux-amd64", f"linux-{arch}"})


def required_servers(selected):
    required = set(common_targets())
    for target in selected:
        required.update((target, "linux-" + target.split("-")[1]))
    return sorted(required)


def build_environment(tag):
    env = os.environ.copy()
    for name in ("GOOS", "GOARCH", "CGO_ENABLED"):
        env.pop(name, None)
    env.update(GOFLAGS="-mod=readonly", PYTHONDONTWRITEBYTECODE="1", SUB2API_BUILD_VERSION=tag.removeprefix("v"), SUB2API_PNPM=PNPM)
    env.setdefault("COREPACK_ENABLE_DOWNLOAD_PROMPT", "0")
    env.setdefault("COREPACK_ENABLE_AUTO_PIN", "0")
    env["COREPACK_ENABLE_PROJECT_SPEC"] = "0"
    return env


def source(root, workspace, tag):
    plugins = discover_plugins(root)
    tree = workspace / "source"
    run(["git", "clone", "--depth", "1", "--branch", tag, UPSTREAM, tree], workspace)
    run(["git", "checkout", "--detach", f"refs/tags/{tag}"], tree)
    apply_patches(tree, plugins)


def server(workspace, tag, target):
    system, arch = target.split("-")
    env = dict(build_environment(tag), GOOS=system, GOARCH=arch, CGO_ENABLED="0")
    binary = workspace / "binaries" / artifact_name(system, arch, tag)
    binary.parent.mkdir(exist_ok=True)
    run(["go", "build", "-tags=embed", "-trimpath", f"-ldflags=-s -w -X main.Version={tag.removeprefix('v')} -X main.BuildType=release", "-o", binary, "./cmd/server"], workspace / "source/backend", env)
    return binary


def embed_payloads(workspace, staging, tag, targets):
    payloads = staging / "payloads"
    if payloads.exists():
        shutil.rmtree(payloads)
    payloads.mkdir()
    for target in sorted(set(targets)):
        system, arch = target.split("-")
        binary = workspace / "binaries" / artifact_name(system, arch, tag)
        with binary.open("rb") as original, gzip.open(payloads / (target + ".gz"), "wb") as compressed:
            shutil.copyfileobj(original, compressed)


def prepare(root, workspace, tag):
    tree = workspace / "source"
    plugins = discover_plugins(root)
    env = build_environment(tag)
    frontend = tree / "frontend"
    settings = json.loads((frontend / "package.json").read_text()).get("pnpm", {})
    # pnpm 12 moved overrides and build settings out of package.json and .npmrc.
    # Keep upstream pnpm 9's dependency-script behavior without changing its lockfile.
    settings.update(ignoreScripts=False, dangerouslyAllowAllBuilds=True, strictPeerDependencies=False)
    (frontend / "pnpm-workspace.yaml").write_text(json.dumps(settings, indent=2) + "\n")
    run(["corepack", "prepare", PNPM, "--activate"], root, env)
    run(["corepack", PNPM, "install", "--frozen-lockfile"], tree / "frontend", env)
    run(["corepack", PNPM, "build"], tree / "frontend", env)
    for plugin in plugins:
        test_entries(plugin, tree, env, common=False)
    run(["go", "vet", "./internal/server", "./internal/server/routes", "./internal/web"], tree / "backend", env)
    staging = prepare_installer(workspace, tree, root, plugins, env)
    for target in common_targets():
        server(workspace, tag, target)
    embed_payloads(workspace, staging, tag, common_targets())
    test_entries(root, tree, env, staging, common=True if root in plugins else None)


def installer(workspace, tag, target):
    system, arch = target.split("-")
    staging = workspace / "installer"
    embed_payloads(workspace, staging, tag, (target, f"linux-{arch}"))
    binary = workspace / "installers" / artifact_name(system, arch, tag, installer=True)
    binary.parent.mkdir(exist_ok=True)
    env = dict(build_environment(tag), GOOS=system, GOARCH=arch, CGO_ENABLED="0")
    run(["go", "build", "-trimpath", f"-ldflags=-s -w -X main.buildVersion={tag.removeprefix('v')}", "-o", binary, "."], staging, env)
    return binary


def publish(root, workspace, tag, selected):
    prepared = workspace / "outputs"
    prepared.mkdir()
    for target in selected:
        system, arch = target.split("-")
        directory = prepared / target
        directory.mkdir()
        for kind, is_installer in (("binaries", False), ("installers", True)):
            name = artifact_name(system, arch, tag, installer=is_installer)
            binary = directory / name
            shutil.copyfile(workspace / kind / name, binary)
            binary.chmod(0o755)
    output = root / "dist/build"
    publish_outputs(prepared, output)
    return sorted(path for path in output.glob("*/*") if path.is_file())


def build(root, workspace, tag, selected=None):
    selected = selected if selected is not None else selected_platforms()
    source(root, workspace, tag)
    return resume(root, workspace, tag, selected)


def resume(root, workspace, tag, selected):
    prepare(root, workspace, tag)
    for target in sorted(set(required_servers(selected)) - set(common_targets())):
        server(workspace, tag, target)
    for target in selected:
        installer(workspace, tag, target)
    return publish(root, workspace, tag, selected)


def stage_outputs(tag, selected):
    outputs = {
        "tag": tag,
        "pnpm": PNPM,
        "platforms": " ".join(selected),
    }
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            for name, value in outputs.items():
                print(f"{name}={value}", file=output)


def run_stage(root, workspace, stage, tag, selected):
    if stage == "source":
        source(root, workspace, tag)
        stage_outputs(tag, selected)
    elif stage == "resume":
        return resume(root, workspace, tag, selected)
    else:
        raise RuntimeError(f"Unknown build stage: {stage}")
    return []


def main():
    argparse.ArgumentParser(description=__doc__).parse_args()
    try:
        stage = os.environ.get("SUB2API_BUILD_STAGE")
        selected = selected_platforms(os.environ.get("SUB2API_BUILD_PLATFORMS"))
        tag = resolve_upstream_tag() if not stage or stage == "source" else os.environ.get("UPSTREAM_TAG")
        if not tag:
            raise RuntimeError("Pass the resolved UPSTREAM_TAG to the resumed build stage.")
        if stage:
            location = os.environ.get("SUB2API_BUILD_WORKSPACE")
            if not location:
                raise RuntimeError("SUB2API_BUILD_WORKSPACE is required for a resumed build stage.")
            workspace = Path(location).resolve()
            workspace.mkdir(parents=True, exist_ok=True)
            outputs = run_stage(ROOT, workspace, stage, tag, selected)
        else:
            with tempfile.TemporaryDirectory(prefix="sub2api-plugins-") as temporary:
                print(f"Build workspace: {temporary}", flush=True)
                outputs = build(ROOT, Path(temporary), tag, selected)
        for output in outputs:
            print(output, flush=True)
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"Build failed: {error}. No new outputs were published.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
