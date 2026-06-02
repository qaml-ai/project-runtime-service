#!/usr/bin/env node
/**
 * publish - Deploy any file as a static Cloudflare Worker with embed viewer
 *
 * Usage:
 *   publish <project-name> --file <path>
 */

import { spawnSync } from 'child_process';
import { writeFileSync, readFileSync, existsSync, mkdirSync, cpSync, statSync } from 'fs';
import { join, basename, resolve } from 'path';

function showHelp() {
  console.log(`
publish - Deploy any file as a static Cloudflare Worker

Usage:
  publish <project-name> --file <path>

Required:
  --file, -f <path>   Path to source file

Options:
  --help               Show this help message

Examples:
  publish sales-report --file ./analysis.ipynb
  publish readme-site --file ./README.md
  publish data-download --file /mnt/user-outputs/results.csv
`);
}

function resolvePath(inputPath) {
  if (inputPath.startsWith('~/')) {
    const home = process.env.HOME || '';
    return resolve(home, inputPath.slice(2));
  }
  return resolve(process.cwd(), inputPath);
}

function parseArgs(args) {
  const result = { projectName: null, filePath: null };

  for (let i = 0; i < args.length; i += 1) {
    const arg = args[i];

    if (arg === '--help' || arg === '-h') {
      return { help: true };
    } else if ((arg === '--file' || arg === '-f') && args[i + 1]) {
      result.filePath = args[++i];
    } else if (!arg.startsWith('-') && !result.projectName) {
      result.projectName = arg;
    }
  }

  return result;
}

function formatTime(ms) {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

function generateWranglerConfig(name) {
  return JSON.stringify(
    {
      $schema: 'node_modules/wrangler/config-schema.json',
      name,
      compatibility_date: '2025-04-04',
      main: './worker.js',
      assets: { directory: './public', binding: 'ASSETS' },
    },
    null,
    '\t'
  );
}

function generateWorkerJs() {
  return `export default {
  async fetch(request, env) {
    let response = await env.ASSETS.fetch(request);
    if (response.status === 404) {
      response = await env.ASSETS.fetch(new Request(new URL('/', request.url), request));
    }
    return response;
  },
};
`;
}

const RENDERER_DIST = '/usr/local/lib/create-worker/renderer-dist';

function copyRendererBundle(projectDir, filename) {
  cpSync(RENDERER_DIST, join(projectDir, 'public'), { recursive: true });
  let html = readFileSync(join(projectDir, 'public', 'index.html'), 'utf-8');
  html = html.replace('</head>', `<script>window.__FILENAME__=${JSON.stringify(filename)}</script>\n</head>`);
  html = html.replace(/<title>[^<]*<\/title>/, `<title>${filename}</title>`);
  writeFileSync(join(projectDir, 'public', 'index.html'), html);
}

async function publish(projectName, filePath) {
  const totalStart = Date.now();
  const projectDir = join(process.cwd(), projectName);
  const sourceFilePath = resolvePath(filePath);
  const filename = basename(sourceFilePath);

  if (!existsSync(sourceFilePath)) {
    console.error(`Error: File not found: ${sourceFilePath}`);
    process.exit(1);
  }

  if (!statSync(sourceFilePath).isFile()) {
    console.error(`Error: Path is not a file: ${sourceFilePath}`);
    process.exit(1);
  }

  const isRedeploy = existsSync(projectDir);
  console.log(`${isRedeploy ? 'Updating' : 'Creating'} project: ${projectName}`);
  console.log(`File: ${sourceFilePath}`);
  console.log('');

  console.log('Step 1/2: Generating project files...');
  let stepStart = Date.now();

  mkdirSync(join(projectDir, 'public'), { recursive: true });
  writeFileSync(join(projectDir, 'wrangler.jsonc'), generateWranglerConfig(projectName));
  writeFileSync(join(projectDir, 'worker.js'), generateWorkerJs());
  copyRendererBundle(projectDir, filename);
  mkdirSync(join(projectDir, 'public', 'files'), { recursive: true });
  cpSync(sourceFilePath, join(projectDir, 'public', 'files', filename), { force: true });

  console.log(`         Done in ${formatTime(Date.now() - stepStart)}`);

  console.log('\nStep 2/2: Deploying...');
  stepStart = Date.now();

  const deployResult = spawnSync(
    'wrangler',
    ['deploy', '--dispatch-namespace', 'chiridion'],
    { cwd: projectDir, stdio: 'inherit', shell: true }
  );

  if (deployResult.status !== 0) {
    console.error(
      '\nDeploy failed. You can retry with:\n  cd ' +
        projectName +
        ' && wrangler deploy --dispatch-namespace chiridion'
    );
    process.exit(1);
  }

  console.log(`         Done in ${formatTime(Date.now() - stepStart)}`);

  const totalTime = Date.now() - totalStart;
  console.log(`
Published in ${formatTime(totalTime)}!

Live URLs:
  https://${projectName}.camelai.app
  https://${projectName}.apps.camelai.dev
`);
}

const args = process.argv.slice(2);
const parsed = parseArgs(args);

if (parsed.help || args.length === 0) {
  showHelp();
  process.exit(0);
}

if (!parsed.projectName) {
  console.error('Error: Project name required');
  showHelp();
  process.exit(1);
}

if (!parsed.filePath) {
  console.error('Error: --file <path> is required');
  showHelp();
  process.exit(1);
}

publish(parsed.projectName, parsed.filePath);
