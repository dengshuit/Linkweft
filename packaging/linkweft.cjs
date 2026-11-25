#!/usr/bin/env node
const { spawn } = require('node:child_process');
const { dirname, join } = require('node:path');
const { name, version, optionalDependencies } = require('./package.json');

function fail(message) {
  console.error(`linkweft: ${message}`);
  process.exitCode = 1;
}

function run() {
  const platform = `${process.platform}-${process.arch}`;
  const platformPackage = `${name}-${platform}`;
  if (!Object.hasOwn(optionalDependencies, platformPackage)) {
    fail(`unsupported platform ${platform}; use a supported operating system and CPU architecture.`);
    return;
  }

  let manifest;
  try {
    manifest = require.resolve(`${platformPackage}/package.json`);
    if (require(manifest).version !== version) {
      manifest = undefined;
    }
  } catch {
    manifest = undefined;
  }
  if (!manifest) {
    fail(`requires ${platformPackage}@${version}; reinstall ${name} with optional dependencies enabled.`);
    return;
  }

  const binary = join(dirname(manifest), process.platform === 'win32' ? 'linkweft.exe' : 'linkweft');
  const child = spawn(binary, process.argv.slice(2), { stdio: 'inherit' });
  process.on('SIGINT', () => {
    // Windows delivers Ctrl+C to the shared console. child.kill would terminate Go immediately there.
    if (process.platform !== 'win32') {
      child.kill('SIGINT');
    }
  });
  child.once('error', () => {
    fail(`could not start the executable from ${platformPackage}; check execute permissions or reinstall ${name}.`);
  });
  child.once('exit', (code, signal) => {
    if (code !== null) {
      process.exitCode = code;
    } else if (signal === 'SIGINT') {
      process.exitCode = 130;
    } else if (signal) {
      fail(`the executable terminated with ${signal}; retry the command.`);
    }
  });
}

run();
