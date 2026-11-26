const { execFileSync, execSync } = require('node:child_process');
const { chmodSync, copyFileSync, existsSync, mkdirSync, mkdtempSync, renameSync, rmSync, writeFileSync } = require('node:fs');
const { basename, join, resolve } = require('node:path');

const scope = process.env.LINKWEFT_NPM_SCOPE || '@linkweft-local';
const repository = process.env.GITHUB_REPOSITORY && process.env.GITHUB_SERVER_URL
  ? { type: 'git', url: `git+${process.env.GITHUB_SERVER_URL}/${process.env.GITHUB_REPOSITORY}.git` }
  : undefined;
const platforms = {
  'linux/amd64': ['linux', 'x64'],
  'linux/arm64': ['linux', 'arm64'],
  'darwin/amd64': ['darwin', 'x64'],
  'darwin/arm64': ['darwin', 'arm64'],
  'windows/amd64': ['win32', 'x64'],
  'windows/arm64': ['win32', 'arm64'],
};

function platformName(os, cpu) {
  return `${scope}/linkweft-${os}-${cpu}`;
}

function validVersion(version) {
  if (!version || version.length > 256) {
    return false;
  }
  const match = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/.exec(version);
  return match !== null && match[0] === version &&
    match.slice(1, 4).every(part => Number.isSafeInteger(Number(part))) &&
    (match[4] === undefined || match[4].split('.').every(part => !/^0[0-9]+$/.test(part)));
}

function pack(directory, manifest) {
  writeFileSync(join(directory, 'package.json'), `${JSON.stringify(manifest, null, 2)}\n`);
  const filename = execSync('npm pack --offline --ignore-scripts --json=false --loglevel=error --pack-destination=.', {
    cwd: directory,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'inherit'],
  }).trim();
  const expected = `${manifest.name.slice(1).replace('/', '-')}-${manifest.version}.tgz`;
  if (filename !== expected) {
    throw new Error(`npm pack did not produce ${expected}`);
  }
  return join(directory, filename);
}

function buildAndPack(version, target, output) {
  const [GOOS, GOARCH] = target.split('/');
  const [os, cpu] = platforms[target];
  const executable = GOOS === 'windows' ? 'linkweft.exe' : 'linkweft';
  const versionDir = join(output, version);
  const destination = join(versionDir, `${GOOS}-${GOARCH}`);
  if (existsSync(destination)) {
    throw new Error(`output already exists at ${destination}; choose a new output directory or version`);
  }
  mkdirSync(versionDir, { recursive: true });
  // Stage on the destination filesystem so a single rename publishes the complete batch.
  const stage = mkdtempSync(join(versionDir, '.linkweft-pack-'));
  try {
    const platform = join(stage, 'platform');
    const main = join(stage, 'main');
    mkdirSync(platform);
    mkdirSync(main);

    const binary = join(platform, executable);
    execFileSync('go', ['build', '-trimpath', '-ldflags', `-X main.version=${version}`, '-o', binary, './cmd/linkweft'], {
      cwd: resolve(__dirname, '..'),
      env: { ...process.env, GOOS, GOARCH, CGO_ENABLED: '0' },
      stdio: 'inherit',
    });
    chmodSync(binary, 0o755);
    const platformArchive = pack(platform, {
      name: platformName(os, cpu),
      version,
      repository,
      os: [os],
      cpu: [cpu],
      files: [executable],
    });

    copyFileSync(join(__dirname, 'linkweft.cjs'), join(main, 'linkweft.cjs'));
    chmodSync(join(main, 'linkweft.cjs'), 0o755);
    const mainArchive = pack(main, {
      name: `${scope}/linkweft`,
      version,
      repository,
      bin: { linkweft: 'linkweft.cjs' },
      engines: { node: '>=24' },
      files: ['linkweft.cjs'],
      optionalDependencies: Object.fromEntries(
        Object.values(platforms).map(([os, cpu]) => [platformName(os, cpu), version]),
      ),
    });

    const batch = join(stage, 'batch');
    mkdirSync(batch);
    const artifacts = [platformArchive, mainArchive, binary];
    for (const artifact of artifacts) {
      renameSync(artifact, join(batch, basename(artifact)));
    }
    renameSync(batch, destination);
    for (const artifact of artifacts) {
      console.log(join(destination, basename(artifact)));
    }
  } finally {
    rmSync(stage, { recursive: true, force: true });
  }
}

const [version, target, output, extra] = process.argv.slice(2);
if (!/^@[a-z0-9][a-z0-9-]{0,191}$/.test(scope)) {
  console.error('LINKWEFT_NPM_SCOPE must be an npm scope such as @linkweft-local.');
  process.exitCode = 2;
} else if (!validVersion(version) || !Object.hasOwn(platforms, target) || !output || extra !== undefined) {
  console.error('Usage: node packaging/pack.cjs VERSION GOOS/GOARCH OUTPUT_DIR');
  console.error('VERSION must be an npm version such as 0.1.0-local.1, without build metadata.');
  console.error(`Targets: ${Object.keys(platforms).join(', ')}`);
  process.exitCode = 2;
} else {
  try {
    buildAndPack(version, target, resolve(output));
  } catch (error) {
    console.error(`linkweft pack: ${error.message}`);
    process.exitCode = 1;
  }
}
