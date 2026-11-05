#!/usr/bin/env python3
"""Check automatic composition and failure propagation with local fixtures."""

import importlib.util
from contextlib import redirect_stderr, redirect_stdout
import io
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("plugin_build", ROOT / "scripts/build.py")
build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build)

BASE = "first\none\ntwo\nthree\nmiddle\nfour\nfive\nsix\nlast\n"
TAG = "v1.2.3-rc.4"


def edit_patch(before, after):
    return (
        "diff --git a/shared.txt b/shared.txt\n--- a/shared.txt\n+++ b/shared.txt\n"
        f"@@ -1,4 +1,4 @@\n-{before}\n+{after}\n one\n two\n three\n"
    )


class BuildTests(unittest.TestCase):
    def fixture(self, directory):
        root = directory / "project"
        root.mkdir()
        (root / "scripts/installer").mkdir(parents=True)
        (root / "scripts/installer/main.go").write_text("package main\nfunc main() {}\n")
        (root / "LICENSE").write_text("common license\n")
        (root / "licenses").mkdir()
        (root / "licenses/notice.txt").write_text("common notice\n")
        for name in ("zeta", "alpha", "template"):
            plugin = root / "plugins" / name
            plugin.mkdir(parents=True)
            subprocess.run(["git", "init", "-q", str(plugin)], check=True)
            subprocess.run(["git", "config", "-f", str(root / ".gitmodules"), f"submodule.{name}.path", f"plugins/{name}"], check=True)
            (plugin / "patches").mkdir()
            (plugin / "LICENSE").write_text(name + " license\n")
            (plugin / "licenses").mkdir()
            (plugin / "licenses/notice.txt").write_text(name + " notice\n")
        alpha = root / "plugins/alpha/patches"
        (alpha / "z.patch").write_text(edit_patch("first-alpha", "first-final"))
        (alpha / "a.patch").write_text(edit_patch("first", "first-alpha"))
        (root / "plugins/zeta/patches/change.patch").write_text(
            "diff --git a/shared.txt b/shared.txt\n--- a/shared.txt\n+++ b/shared.txt\n"
            "@@ -6,4 +6,4 @@\n-four\n+four-zeta\n five\n six\n last\n"
        )
        for owner in (root, root / "plugins/alpha"):
            (owner / "tests").mkdir()
            (owner / "assets").mkdir()
            (owner / "assets/fixture.json").write_text('{"value":21}\n')
            (owner / "tests/test_a.py").write_text(
                "import os\nfrom pathlib import Path\n"
                "source = Path(os.environ['SUB2API_SOURCE'])\n"
                "assert 'first-final' in (source / 'shared.txt').read_text()\n"
                "assert 'four-zeta' in (source / 'shared.txt').read_text()\n"
                "assert not os.environ.get('GOOS')\n"
                "assert os.environ['SUB2API_BUILD_VERSION'] == '1.2.3-rc.4'\n"
                "assert (Path(os.environ['SUB2API_ASSETS']) / 'fixture.json').is_file()\n"
                f"with open({str(root / 'events')!r}, 'a') as log: log.write({owner.name!r} + ':python\\n')\n"
            )
            (owner / "tests/test_b.go").write_text(
                'package main\nimport ("os"; "fmt")\nfunc main() {\n'
                f' f, err := os.OpenFile({str(root / "events")!r}, os.O_APPEND|os.O_WRONLY, 0600)\n'.replace("'", '"')
                + f' if err != nil {{ panic(err) }}; defer f.Close(); fmt.Fprintln(f, "{owner.name}:go")\n}}\n'
            )
        event_path = json.dumps(str(root / "events"))
        native = root / "plugins/alpha/tests"
        (native / "test_c_first_test.go").write_text(
            "// sub2api-package: backend/internal/eval\n"
            'package eval\nimport ("fmt"; "os"; "testing")\n'
            "func TestFirst(t *testing.T) {\n"
            ' if value() != 21 || upstreamPrivateFixture() != 21 { t.Fatal("private package context was lost") }\n'
            ' record(t, "alpha:native-first")\n}\n'
            "func record(t *testing.T, value string) {\n"
            f" f, err := os.OpenFile({event_path}, os.O_APPEND|os.O_WRONLY, 0600)\n"
            " if err != nil { t.Fatal(err) }; defer f.Close(); fmt.Fprintln(f, value)\n}\n"
        )
        (native / "test_c_second_test.go").write_text(
            "// sub2api-package: backend/internal/eval\n"
            'package eval\nimport "testing"\n'
            'func TestSecond(t *testing.T) { record(t, "alpha:native-second") }\n'
        )
        (native / "test_d_ui.cjs").write_text(
            'const assert = require("node:assert/strict");\n'
            'assert.ok(process.env.SUB2API_SOURCE);\n'
            f'require("node:fs").appendFileSync({event_path}, "alpha:node\\n");\n'
        )
        (native / "test_e_quota.test.ts").write_text("import './native_fixture'\n")
        (native / "native_fixture.ts").write_text("export const fixture = 21\n")
        (native / "test_build.py").write_text("raise AssertionError('duplicate common build test')\n")
        (native / "test_installer_test.go").write_text("duplicate common installer test\n")
        template = root / "plugins/template/tests"
        template.mkdir()
        (template / "test_a.py").write_text("raise AssertionError('empty template participated')\n")
        seed = directory / "seed"
        (seed / "backend").mkdir(parents=True)
        (seed / "frontend").mkdir()
        (seed / "frontend/src").mkdir()
        (seed / "frontend/package.json").write_text('{"pnpm":{"overrides":{"fixture":"1.0.0"}}}\n')
        (seed / "backend/go.mod").write_text("module fixture\n\ngo 1.27.0\n")
        (seed / "backend/internal/eval").mkdir(parents=True)
        (seed / "backend/internal/eval/eval.go").write_text("package eval\nfunc value() int { return 21 }\n")
        (seed / "backend/internal/eval/unrelated_test.go").write_text(
            'package eval\nimport "testing"\n'
            "func upstreamPrivateFixture() int { return 21 }\n"
            'func TestUnrelated(t *testing.T) { t.Fatal("unrelated upstream suite executed") }\n'
        )
        (seed / "shared.txt").write_text(BASE)
        output = root / "dist/build"
        output.mkdir(parents=True)
        (output / "linux-amd64").mkdir()
        for name in (build.artifact_name("linux", "amd64", TAG), build.artifact_name("linux", "amd64", TAG, installer=True)):
            (output / "linux-amd64" / name).write_text("previous " + name)
        return root, seed

    def runner(self, seed, calls, failure=None, output=None):
        real_run = build.run

        def run(argv, cwd, env=None):
            args = list(map(str, argv))
            calls.append(args)
            if args[:2] == ["git", "clone"]:
                if failure == "source":
                    raise subprocess.CalledProcessError(1, args)
                self.assertEqual(args[2:6], ["--depth", "1", "--branch", TAG])
                shutil.copytree(seed, Path(args[-1]))
            elif args[:2] == ["git", "checkout"]:
                self.assertEqual(args[2:], ["--detach", f"refs/tags/{TAG}"])
            elif args[0] == "corepack":
                if failure == "dependency":
                    raise subprocess.CalledProcessError(1, args)
                if args[2:5] == ["exec", "vitest", "run"]:
                    suite = Path(cwd) / args[5]
                    self.assertEqual(suite.read_text(), "import './native_fixture'\n")
                    self.assertTrue((suite.parent / "native_fixture.ts").is_file())
                    if failure == "vue":
                        raise subprocess.CalledProcessError(1, args)
                    with (Path(cwd).parents[2] / "project/events").open("a") as log:
                        log.write("alpha:vue\n")
            elif args[:2] == ["go", "vet"]:
                self.assertEqual(args[2:], ["./internal/server", "./internal/server/routes", "./internal/web"])
            elif args[:2] in (["go", "get"], ["go", "mod"]):
                pass
            elif args[:2] == ["go", "build"]:
                self.assertIn((env["GOOS"], env["GOARCH"]), build.TARGETS)
                self.assertEqual(env["CGO_ENABLED"], "0")
                metadata = "main.buildVersion" if args[-1] == "." else "main.Version"
                self.assertIn(f"{metadata}=1.2.3-rc.4", next(arg for arg in args if arg.startswith("-ldflags=")))
                if args[-1] == ".":
                    selected = sorted(path.name for path in (Path(cwd) / "payloads").iterdir())
                    self.assertEqual(selected, sorted({f'{env["GOOS"]}-{env["GOARCH"]}.gz', f'linux-{env["GOARCH"]}.gz'}))
                Path(args[args.index("-o") + 1]).write_bytes(b"built output")
            else:
                if output is None:
                    real_run(argv, cwd, env)
                else:
                    print("+ " + " ".join(args), flush=True)
                    subprocess.run(args, cwd=cwd, env=env, stdout=output, stderr=subprocess.STDOUT, check=True)

        return run

    def test_combined_build(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            root, seed = self.fixture(directory)
            workspace = directory / "workspace"
            workspace.mkdir()
            calls = []
            response = io.BytesIO(json.dumps({"tag_name": TAG}).encode())
            with patch.dict(build.os.environ, {"UPSTREAM_TAG": ""}), patch.object(build.urllib.request, "urlopen", return_value=response) as lookup:
                tag = build.resolve_upstream_tag()
                lookup.assert_called_once()
                request = lookup.call_args.args[0]
                self.assertEqual(request.full_url, "https://api.github.com/repos/Wei-Shaw/sub2api/releases/latest")
                self.assertFalse(request.has_header("Authorization"))
            with patch.object(build, "run", self.runner(seed, calls)):
                artifacts = build.build(root, workspace, tag)
            self.assertEqual([item.read_bytes() for item in artifacts], [b"built output"] * 10)
            expected = {
                f"{system}-{arch}/sub2api-plugins{'-installer' if installer else ''}-{TAG}-{system}-{arch}{'.exe' if system == 'windows' else ''}"
                for system, arch in build.TARGETS for installer in (False, True)
            }
            self.assertEqual({item.relative_to(root / "dist/build").as_posix() for item in artifacts}, expected)
            self.assertEqual((root / "events").read_text().splitlines(), [
                "alpha:python", "alpha:go", "alpha:native-first", "alpha:native-second",
                "alpha:node", "alpha:vue", "project:python", "project:go",
            ])
            native_calls = [call for call in calls if call[:2] == ["go", "test"]]
            self.assertEqual(len(native_calls), 1)
            self.assertEqual(native_calls[0][native_calls[0].index("-run") + 1], "^(TestFirst|TestSecond)$")
            self.assertFalse((workspace / "source/backend/internal/eval/test_c_first_test.go").exists())
            self.assertEqual(list((workspace / "source/frontend/src").iterdir()), [])
            patches = [Path(call[-1]).name for call in calls if call[:2] == ["git", "apply"]]
            self.assertEqual(patches, ["a.patch", "z.patch", "change.patch"])
            self.assertEqual(sum(call[:2] == ["go", "build"] and call[-1] == "." for call in calls), len(build.TARGETS))
            for namespace, owner in (("", "common"), ("alpha/", "alpha"), ("zeta/", "zeta")):
                license_dir = workspace / "installer/licenses" / namespace
                self.assertEqual((license_dir / "LICENSE").read_text(), owner + " license\n")
                self.assertEqual((license_dir / "notice.txt").read_text(), owner + " notice\n")
            self.assertEqual((seed / "shared.txt").read_text(), BASE)
            self.assertFalse((workspace / "installer/licenses/template").exists())
            settings = json.loads((workspace / "source/frontend/pnpm-workspace.yaml").read_text())
            self.assertEqual(settings["overrides"], {"fixture": "1.0.0"})
            self.assertTrue(settings["dangerouslyAllowAllBuilds"])

    def test_standalone_build(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            root, seed = self.fixture(directory)
            plugin = root / "plugins/alpha"
            (plugin / "scripts/installer").mkdir(parents=True)
            shutil.copyfile(root / "scripts/installer/main.go", plugin / "scripts/installer/main.go")
            (plugin / "tests/test_a.py").write_text(
                "import os\nfrom pathlib import Path\n"
                "source = Path(os.environ['SUB2API_SOURCE'])\n"
                "assert 'first-final' in (source / 'shared.txt').read_text()\n"
                "assert 'four-zeta' not in (source / 'shared.txt').read_text()\n"
                f"with open({str(root / 'events')!r}, 'a') as log: log.write('alpha:python\\n')\n"
            )
            (plugin / "tests/test_build.py").write_text(
                f"with open({str(root / 'events')!r}, 'a') as log: log.write('alpha:common-build\\n')\n"
            )
            (plugin / "tests/test_installer_test.go").write_text(
                "// sub2api-package: installer\n"
                'package main\nimport ("fmt"; "os"; "testing")\n'
                "func TestCommonInstaller(t *testing.T) {\n"
                f" f, err := os.OpenFile({json.dumps(str(root / 'events'))}, os.O_APPEND|os.O_WRONLY, 0600)\n"
                ' if err != nil { t.Fatal(err) }; defer f.Close(); fmt.Fprintln(f, "alpha:common-installer")\n}\n'
            )
            workspace = directory / "workspace"
            workspace.mkdir()
            calls = []
            with patch.dict(build.os.environ, {"UPSTREAM_TAG": TAG}), patch.object(build.urllib.request, "urlopen") as lookup:
                tag = build.resolve_upstream_tag()
                lookup.assert_not_called()
            with patch.object(build, "run", self.runner(seed, calls)):
                artifacts = build.build(plugin, workspace, tag)
            self.assertEqual([item.read_bytes() for item in artifacts], [b"built output"] * 10)
            self.assertEqual((root / "events").read_text().splitlines(), [
                "alpha:python", "alpha:go", "alpha:native-first", "alpha:native-second",
                "alpha:node", "alpha:vue", "alpha:common-build", "alpha:common-installer",
            ])
            patches = [Path(call[-1]).name for call in calls if call[:2] == ["git", "apply"]]
            self.assertEqual(patches, ["a.patch", "z.patch"])
            self.assertEqual((workspace / "installer/licenses/LICENSE").read_text(), "alpha license\n")
            self.assertFalse((workspace / "installer/licenses/alpha").exists())

    def test_failures_stop_publication(self):
        for failure in ("release", "source", "dependency", "patch", "test", "native", "vue"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                root, seed = self.fixture(directory)
                if failure == "patch":
                    (root / "plugins/alpha/patches/a.patch").write_text(edit_patch("wrong original", "rejected"))
                if failure == "test":
                    (root / "plugins/alpha/tests/test_a.py").write_text("raise SystemExit(17)\n")
                if failure == "native":
                    native = root / "plugins/alpha/tests/test_c_first_test.go"
                    native.write_text(native.read_text().replace('record(t, "alpha:native-first")', 't.Fatal("native fixture failure")'))
                workspace = directory / "workspace"
                workspace.mkdir()
                calls = []
                with tempfile.TemporaryFile(mode="w+") as output:
                    try:
                        with redirect_stdout(output), redirect_stderr(output), patch.object(build, "run", self.runner(seed, calls, failure, output)), patch.dict(build.os.environ, {"UPSTREAM_TAG": ""}), patch.object(build.urllib.request, "urlopen", side_effect=OSError("Release lookup failed")):
                            with self.assertRaises((RuntimeError, subprocess.CalledProcessError)) as caught:
                                tag = build.resolve_upstream_tag() if failure == "release" else TAG
                                build.build(root, workspace, tag)
                        if failure == "patch":
                            self.assertIn("alpha/a.patch", str(caught.exception))
                        self.assertFalse(any(call[:2] == ["go", "build"] for call in calls))
                        if failure == "patch":
                            self.assertFalse(any(Path(call[-1]).name == "z.patch" for call in calls))
                        if failure == "test":
                            self.assertFalse(any(Path(call[-1]).name == "test_b.go" for call in calls))
                        if failure == "native":
                            self.assertNotIn("alpha:native-second", (root / "events").read_text())
                            self.assertFalse(any(call[0] == "node" for call in calls))
                        if failure == "vue":
                            self.assertEqual(list((workspace / "source/frontend/src").iterdir()), [])
                        for name in (build.artifact_name("linux", "amd64", TAG), build.artifact_name("linux", "amd64", TAG, installer=True)):
                            self.assertEqual((root / "dist/build/linux-amd64" / name).read_text(), "previous " + name)
                    except Exception:
                        output.seek(0)
                        print(output.read(), file=sys.stderr)
                        raise

        for failure in ("partial-copy", "publish-rename"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                root, _ = self.fixture(directory)
                prepared = directory / "prepared"
                prepared.mkdir()
                (prepared / "new-program").write_text("new output")
                copytree, rename = shutil.copytree, Path.rename

                def copy(source, destination, *args, **kwargs):
                    if failure == "partial-copy":
                        Path(destination).mkdir()
                        (Path(destination) / "partial").write_text("partial output")
                        raise OSError("Synthetic cross-filesystem copy failure")
                    return copytree(source, destination, *args, **kwargs)

                def move(source, destination):
                    if source.name == "candidate":
                        raise OSError("Synthetic publication rename failure")
                    return rename(source, destination)

                with patch.object(build.shutil, "copytree", copy), patch.object(Path, "rename", move):
                    with self.assertRaises(OSError):
                        build.publish_outputs(prepared, root / "dist/build")
                for name in (build.artifact_name("linux", "amd64", TAG), build.artifact_name("linux", "amd64", TAG, installer=True)):
                    self.assertEqual((root / "dist/build/linux-amd64" / name).read_text(), "previous " + name)

    def test_uninitialized_submodule(self):
        with tempfile.TemporaryDirectory() as temporary:
            root, _ = self.fixture(Path(temporary))
            shutil.rmtree(root / "plugins/alpha/.git")
            with self.assertRaisesRegex(RuntimeError, "git submodule update --init --recursive -- plugins/alpha"):
                build.discover_plugins(root)


if __name__ == "__main__":
    unittest.main()
