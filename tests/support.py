"""Execute flat test files in their native temporary build contexts."""

import re
import shutil
import tempfile
from pathlib import Path

COMMON_TESTS = {"test_build.py", "test_installer_test.go"}


def test_entries(owner, tree, env, run, installer=None, common=None):
    context = dict(env, SUB2API_SOURCE=str(tree), SUB2API_ASSETS=str(owner / "assets"))
    if installer is not None:
        context["SUB2API_INSTALLER_SOURCE"] = str(installer)
    entries = sorted(path for path in (owner / "tests").glob("test_*") if path.is_file() and (path.suffix in (".py", ".go", ".cjs") or path.name.endswith(".test.ts")))
    if common is not None:
        entries = [path for path in entries if (path.name in COMMON_TESTS) == common]
    groups = {}
    targets = {}
    for path in entries:
        if not path.name.endswith("_test.go"):
            continue
        source = path.read_text()
        marker = re.search(r"^// sub2api-package: (.+)$", source, re.MULTILINE)
        if marker is None:
            raise RuntimeError(f"Declare // sub2api-package: <native path> in {path}")
        target = marker.group(1).strip()
        targets[path] = target
        groups.setdefault(target, []).append(path)
    completed = set()
    for path in entries:
        if path.name.endswith(".test.ts"):
            if "vue" in completed:
                continue
            frontend = tree / "frontend"
            with tempfile.TemporaryDirectory(prefix="plugin-tests-", dir=frontend / "src") as temporary:
                package = Path(temporary)
                for source in (owner / "tests").glob("*.ts"):
                    shutil.copyfile(source, package / source.name)
                suites = [package / suite.name for suite in entries if suite.name.endswith(".test.ts")]
                run(["corepack", context["SUB2API_PNPM"], "exec", "vitest", "run", *[suite.relative_to(frontend) for suite in suites]], frontend, context)
            completed.add("vue")
            continue
        if path not in targets:
            command = {".py": ["python3"], ".go": ["go", "run"], ".cjs": ["node"]}[path.suffix]
            run([*command, path], owner, context)
            continue
        target = targets[path]
        if target in completed:
            continue
        package = installer if target == "installer" else tree / target
        if package is None:
            raise RuntimeError(f"Installer context is required for {path}")
        copied = []
        names = []
        try:
            for source in groups[target]:
                destination = package / source.name
                shutil.copyfile(source, destination)
                copied.append(destination)
                names.extend(name for name in re.findall(r"^func\s+(Test\w+)\s*\(", source.read_text(), re.MULTILINE) if name != "TestMain")
            pattern = "^(" + "|".join(dict.fromkeys(names)) + ")$"
            run(["go", "test", "-tags=unit", "-failfast", "-run", pattern, "."], package, context)
        finally:
            for destination in copied:
                destination.unlink()
        completed.add(target)
