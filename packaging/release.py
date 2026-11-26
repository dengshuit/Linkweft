import argparse
import base64
from contextlib import contextmanager
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from threading import Thread
from urllib.parse import unquote, urlsplit


TARGETS = {
    "linux-amd64": ("linux", "x64"),
    "linux-arm64": ("linux", "arm64"),
    "darwin-amd64": ("darwin", "x64"),
    "darwin-arm64": ("darwin", "arm64"),
    "windows-amd64": ("win32", "x64"),
    "windows-arm64": ("win32", "arm64"),
}
PROCESS_TESTS = "^(TestCLIProcessStreams|TestCLIConfigInitAC40|TestCLIInitCommandR21|TestCLIPersistentConfig)$"
LOCAL_SCOPE = "@linkweft-local"
STABLE_VERSION = re.compile(r"(?:0|[1-9][0-9]*)(?:\.(?:0|[1-9][0-9]*)){2}")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def executable(target):
    return "linkweft.exe" if target.startswith("windows-") else "linkweft"


def binary_path(target, version):
    return Path("bin") / f"linkweft-{version}-{target}{Path(executable(target)).suffix}"


def archives(scope, version):
    main = f"{scope}/linkweft"
    names = [f"{main}-{'-'.join(platform)}" for platform in TARGETS.values()]
    return {name: Path("npm") / f"{name[1:].replace('/', '-')}-{version}.tgz" for name in [*names, main]}


def manifest(root, version, commit, scope):
    files = [binary_path(target, version) for target in TARGETS]
    files.extend(archives(scope, version).values())
    return {
        "version": version,
        "commit": commit,
        "scope": scope,
        "sha256": {path.as_posix(): hashlib.sha256((root / path).read_bytes()).hexdigest() for path in files},
    }


def packument(archive, payload):
    with tarfile.open(archive) as package:
        require(set(package.getnames()) == {"package/package.json", f"package/{payload}"},
                f"unexpected archive contents in {archive.name}")
        require(package.getmember(f"package/{payload}").mode & 0o111,
                f"{payload} is not executable in {archive.name}")
        return json.load(package.extractfile("package/package.json")), package.extractfile(f"package/{payload}").read()


def assemble(batches, root, version, commit, scope):
    (root / "bin").mkdir(parents=True)
    (root / "npm").mkdir()
    packages = archives(scope, version)
    main = f"{scope}/linkweft"
    main_path = root / packages[main]
    for target, (os_name, cpu) in TARGETS.items():
        batch = batches / version / target
        binary = batch / executable(target)
        name = f"{main}-{os_name}-{cpu}"
        archive = batch / packages[name].name
        metadata, payload = packument(archive, binary.name)
        require((metadata["name"], metadata["version"], metadata["os"], metadata["cpu"])
                == (name, version, [os_name], [cpu]), f"incorrect metadata for {target}")
        require(payload == binary.read_bytes(), f"binary differs from npm archive for {target}")
        shutil.copyfile(binary, root / binary_path(target, version))
        shutil.copyfile(archive, root / packages[name])

        main_archive = batch / main_path.name
        if main_path.exists():
            require(main_path.read_bytes() == main_archive.read_bytes(), f"main package differs for {target}")
        else:
            shutil.copyfile(main_archive, main_path)

    metadata, launcher = packument(main_path, "linkweft.cjs")
    require(metadata["name"] == main and metadata["version"] == version, "incorrect main package identity")
    require(metadata["bin"] == {"linkweft": "linkweft.cjs"} and metadata["engines"] == {"node": ">=24"},
            "incorrect main package entry point or Node version")
    require(metadata["optionalDependencies"] == {name: version for name in packages if name != main},
            "incorrect optional platform dependencies")
    require(launcher == Path(__file__).with_name("linkweft.cjs").read_bytes(), "incorrect launcher contents")
    (root / "manifest.json").write_text(json.dumps(manifest(root, version, commit, scope), indent=2) + "\n")


@contextmanager
def registry(root, packages, version):
    tarballs = {path.name: root / path for path in packages.values()}
    payloads = {name: executable(target) for name, target in zip(packages, TARGETS)}
    payloads[next(reversed(packages))] = "linkweft.cjs"

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            path = unquote(urlsplit(self.path).path).lstrip("/")
            if path in packages:
                archive = root / packages[path]
                metadata, _ = packument(archive, payloads[path])
                metadata["dist"] = {"tarball": url + "/" + archive.name, "integrity": integrity(archive)}
                data = json.dumps({"name": path, "dist-tags": {"latest": version},
                                   "versions": {version: metadata}}).encode()
                content_type = "application/json"
            elif path in tarballs:
                data = tarballs[path].read_bytes()
                content_type = "application/octet-stream"
            else:
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    with ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
        url = f"http://127.0.0.1:{server.server_port}"
        thread = Thread(target=server.serve_forever)
        thread.start()
        try:
            yield url
        finally:
            server.shutdown()
            thread.join()


def run(*args, **kwargs):
    return subprocess.run(args, check=True, encoding="utf-8", timeout=300, **kwargs)


def verify(root, version, scope, target):
    main = f"{scope}/linkweft"
    os_name, cpu = TARGETS[target]
    npm = shutil.which("npm")
    native = json.loads(run("node", "-p", "JSON.stringify([process.platform, process.arch])",
                            capture_output=True).stdout)
    require(native == [os_name, cpu], f"expected native Node for {target}, found {native}")
    with tempfile.TemporaryDirectory(prefix="linkweft-accept-") as temp:
        directory = Path(temp)
        prefix = directory / "npm prefix"
        userconfig, globalconfig = directory / "user.npmrc", directory / "global.npmrc"
        userconfig.touch()
        globalconfig.touch()
        packages = archives(scope, version)
        with registry(root, packages, version) as url:
            run(npm, "install", "--global", "--prefix", str(prefix), "--registry", url,
                "--cache", str(directory / "cache"), "--userconfig", str(userconfig),
                "--globalconfig", str(globalconfig), "--no-audit", "--no-fund", "--no-update-notifier",
                "--ignore-scripts", "--fetch-retries=0", "--include=optional", f"{main}@{version}")

        windows = os_name == "win32"
        installed = prefix / ("node_modules" if windows else "lib/node_modules") / scope
        launcher = installed / "linkweft" / "linkweft.cjs"
        platform = f"{main}-{os_name}-{cpu}"
        resolved = json.loads(run("node", "-e", """
          const requireFromLauncher = require('node:module').createRequire(process.argv[1]);
          const manifest = requireFromLauncher('./package.json');
          const installed = {};
          for (const name of Object.keys(manifest.optionalDependencies)) {
            try {
              installed[name] = requireFromLauncher.resolve(name + '/package.json');
            } catch (error) {
              if (error.code !== 'MODULE_NOT_FOUND') throw error;
            }
          }
          console.log(JSON.stringify(installed));
        """, str(launcher), capture_output=True).stdout)
        require(set(resolved) == {platform},
                f"npm did not select only the {target} platform package")
        binary = directory / executable(target)
        shutil.copyfile(root / binary_path(target, version), binary)
        binary.chmod(0o755)  # Actions artifact downloads do not preserve executable bits.
        require(Path(resolved[platform]).with_name(executable(target)).read_bytes() == binary.read_bytes(),
                f"installed binary differs for {target}")
        command = prefix / ("linkweft.cmd" if windows else "bin/linkweft")
        for entry in (command, binary):
            result = run(str(entry), "--version", capture_output=True)
            require(result.stdout == version + "\n" and not result.stderr, f"unexpected version from {entry}")

        env = dict(os.environ, LINKWEFT_TEST_BINARY=str(binary), LINKWEFT_TEST_NODE_LAUNCHER="")
        tests = ("go", "test", "-count=1", "-v", "./cmd/linkweft", "-run", PROCESS_TESTS)
        run(*tests, env=env)
        env["LINKWEFT_TEST_NODE_LAUNCHER"] = str(launcher)
        run(*tests, env=env)


def integrity(archive):
    return "sha512-" + base64.b64encode(hashlib.sha512(archive.read_bytes()).digest()).decode()


def npm_view(package, field):
    result = subprocess.run(("npm", "view", package, field, "--json", "--prefer-online"),
                            capture_output=True, encoding="utf-8", timeout=60)
    data = json.loads(result.stdout) if result.stdout.strip() else None
    if result.returncode:
        error = data["error"]["code"] if data else result.returncode
        require(error == "E404", f"cannot check {package}: {error}")
        return None
    return data


def publish_npm(root, version, scope):
    npm_registry = run("npm", "config", "get", "registry", capture_output=True).stdout.strip()
    require(scope != LOCAL_SCOPE or urlsplit(npm_registry).hostname == "127.0.0.1",
            "the local npm scope requires a loopback registry")
    current = tuple(map(int, version.split(".")))
    # archives() orders the six platform packages before the main package.
    for name, path in archives(scope, version).items():
        archive = root / path
        digest = npm_view(f"{name}@{version}", "dist.integrity")
        if digest is not None:
            expected = integrity(archive)
            require(digest == expected or digest == [expected], f"published archive differs for {name}@{version}")
            print(f"Already published with matching integrity: {name}@{version}", flush=True)
            continue

        # npm view treats @* as the default tag, so use an explicit stable range.
        versions = npm_view(f"{name}@>=0.0.0", "version")
        if isinstance(versions, str):
            versions = [versions]
        newer = any(tuple(map(int, published.split("."))) > current
                    for published in versions or [] if STABLE_VERSION.fullmatch(published))
        dist_tag = "backfill" if newer else "latest"
        run("npm", "publish", str(archive), "--access", "public", "--ignore-scripts", "--tag", dist_tag)


def verify_tag(tag, commit):
    repository = os.environ["GITHUB_REPOSITORY"]
    actual = run("gh", "api", f"repos/{repository}/commits/refs/tags/{tag}",
                 "--jq", ".sha", capture_output=True).stdout.strip()
    require(actual == commit, f"{tag} does not resolve to the accepted commit {commit}")


def publish_release(root, version, commit, scope):
    tag = f"v{version}"
    require(STABLE_VERSION.fullmatch(version), "publication requires a stable version")
    require(os.environ.get("GITHUB_EVENT_NAME") == "push" and os.environ.get("GITHUB_REF") == f"refs/tags/{tag}",
            "publication requires a push of the matching version tag")
    require(os.environ.get("GITHUB_REF_PROTECTED") == "true",
            "release tag must be protected; enable the tag update and deletion ruleset before publishing")
    verify_tag(tag, commit)
    publish_npm(root, version, scope)
    verify_tag(tag, commit)

    assets = [root / binary_path(target, version) for target in TARGETS]
    release = subprocess.run(("gh", "release", "view", tag, "--json", "isDraft,assets"),
                             capture_output=True, encoding="utf-8", timeout=60)
    if release.returncode:
        run("gh", "release", "create", tag, "--verify-tag", "--generate-notes", *assets)
        return
    existing = json.loads(release.stdout)
    by_name = {asset["name"]: asset for asset in existing["assets"]}
    missing = []
    starters = []
    with tempfile.TemporaryDirectory(prefix="linkweft-assets-") as temp:
        for asset in assets:
            remote = by_name.get(asset.name)
            if remote is None:
                missing.append(asset)
                continue
            if remote.get("state") == "starter" and remote.get("size") == 0:
                asset_id = remote["apiUrl"].rsplit("/", 1)[-1]
                require(re.fullmatch(r"[0-9]+", asset_id), f"invalid GitHub attachment ID: {asset.name}")
                starters.append(asset_id)
                missing.append(asset)
                continue
            run("gh", "release", "download", tag, "--pattern", asset.name, "--dir", temp)
            require((Path(temp) / asset.name).read_bytes() == asset.read_bytes(),
                    f"published GitHub attachment differs: {asset.name}")
    for asset_id in starters:
        run("gh", "api", "--method", "DELETE", f"repos/{os.environ['GITHUB_REPOSITORY']}/releases/assets/{asset_id}")
    if missing:
        run("gh", "release", "upload", tag, *missing)
    if existing["isDraft"]:
        run("gh", "release", "edit", tag, "--draft=false")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("version")
    parser.add_argument("commit")
    parser.add_argument("root", type=Path)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("assemble").add_argument("batches", type=Path)
    commands.add_parser("verify").add_argument("target", choices=TARGETS)
    commands.add_parser("publish")
    args = parser.parse_args()
    scope = os.environ.get("LINKWEFT_NPM_SCOPE") or LOCAL_SCOPE
    if args.command == "assemble":
        assemble(args.batches, args.root, args.version, args.commit, scope)
    else:
        expected = manifest(args.root, args.version, args.commit, scope)
        require(json.loads((args.root / "manifest.json").read_text()) == expected, "artifact manifest mismatch")
        if args.command == "verify":
            verify(args.root, args.version, scope, args.target)
        else:
            publish_release(args.root, args.version, args.commit, scope)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        sys.exit(str(error))
