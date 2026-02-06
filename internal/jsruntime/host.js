#!/usr/bin/env bun
// host.js - Bun host script for Nupi JS runtime
// Receives JSON-RPC commands via stdin, responds via stdout
// Supports: loadPlugin, call, shutdown

const { readSync, readFileSync } = require('node:fs');
const { StringDecoder } = require('node:string_decoder');

// Redirect console.log/warn/error to stderr to prevent plugin logging
// from corrupting the JSON-RPC protocol on stdout
const originalConsole = { ...console };
console.log = (...args) => {
  process.stderr.write('[plugin:log] ' + args.map(a =>
    typeof a === 'object' ? JSON.stringify(a) : String(a)
  ).join(' ') + '\n');
};
console.warn = (...args) => {
  process.stderr.write('[plugin:warn] ' + args.map(a =>
    typeof a === 'object' ? JSON.stringify(a) : String(a)
  ).join(' ') + '\n');
};
console.error = (...args) => {
  process.stderr.write('[plugin:error] ' + args.map(a =>
    typeof a === 'object' ? JSON.stringify(a) : String(a)
  ).join(' ') + '\n');
};

// Protocol output - only this should write to stdout
function sendResponse(response) {
  process.stdout.write(JSON.stringify(response) + '\n');
}

// Timeout helper - wraps a promise with a timeout
function withTimeout(promise, ms, message = 'Operation timed out') {
  if (!ms || ms <= 0) {
    return promise; // No timeout
  }

  return Promise.race([
    promise,
    new Promise((_, reject) =>
      setTimeout(() => reject(new Error(`${message} after ${ms}ms`)), ms)
    )
  ]);
}

// Default timeout for plugin calls (10 seconds)
const DEFAULT_CALL_TIMEOUT_MS = 10000;

const plugins = new Map();

async function loadPlugin(path, options = {}) {
  try {
    // Read file content synchronously to avoid Bun async stdin buffering issues
    const source = readFileSync(path, 'utf8');

    // Create a module-like environment
    const exports = {};
    const module = { exports };

    // Execute the plugin code
    const fn = new Function('module', 'exports', 'require', source);
    fn(module, exports, (id) => {
      throw new Error(`require() not supported: ${id}`);
    });

    // Get the actual exports (handle both module.exports = ... and exports.xxx = ...)
    const pluginExports = module.exports !== exports ? module.exports : exports;

    // Validate required functions if specified
    const { requireFunctions = [] } = options;
    const exports_info = {
      hasTransform: typeof pluginExports.transform === 'function',
      hasDetect: typeof pluginExports.detect === 'function',
      // Tool processor capabilities (SESSION_OUTPUT flow)
      hasDetectIdleState: typeof pluginExports.detectIdleState === 'function',
      hasClean: typeof pluginExports.clean === 'function',
      hasExtractEvents: typeof pluginExports.extractEvents === 'function',
    };

    for (const fnName of requireFunctions) {
      if (typeof pluginExports[fnName] !== 'function') {
        throw new Error(`Plugin ${path} is missing required function: ${fnName}`);
      }
    }

    // Store the plugin
    plugins.set(path, {
      source,
      exports: pluginExports,
    });

    // Return metadata with validation info
    return {
      name: pluginExports.name || path.split('/').pop().replace(/\.[jt]s$/, ''),
      commands: Array.isArray(pluginExports.commands) ? pluginExports.commands : [],
      icon: pluginExports.icon || '',
      ...exports_info,
    };
  } catch (err) {
    throw new Error(`Failed to load plugin ${path}: ${err.message}`);
  }
}

async function callFunction(pluginPath, fnName, args, timeoutMs) {
  const plugin = plugins.get(pluginPath);
  if (!plugin) {
    throw new Error(`Plugin not loaded: ${pluginPath}`);
  }

  const fn = plugin.exports[fnName];
  if (typeof fn !== 'function') {
    throw new Error(`${fnName} is not a function in ${pluginPath}`);
  }

  // Call the function with provided arguments and timeout
  // Bind `this` to plugin.exports so methods can call other methods (e.g., this.clean())
  const timeout = timeoutMs || DEFAULT_CALL_TIMEOUT_MS;
  const result = await withTimeout(
    fn.call(plugin.exports, ...(args || [])),
    timeout,
    `Plugin ${pluginPath}.${fnName} timed out`
  );
  return result;
}

async function handleRequest(req) {
  const { id, method, params } = req;

  try {
    let result;

    switch (method) {
      case 'loadPlugin': {
        if (!params?.path) {
          throw new Error('loadPlugin requires path parameter');
        }
        result = await loadPlugin(params.path, {
          requireFunctions: params.requireFunctions || [],
        });
        break;
      }

      case 'call': {
        if (!params?.plugin || !params?.fn) {
          throw new Error('call requires plugin and fn parameters');
        }
        result = await callFunction(params.plugin, params.fn, params.args, params.timeout);
        break;
      }

      case 'shutdown': {
        // Send response before exiting
        sendResponse({ id, result: { ok: true } });
        process.exit(0);
      }

      case 'ping': {
        result = { pong: true, plugins: plugins.size };
        break;
      }

      default:
        throw new Error(`Unknown method: ${method}`);
    }

    return { id, result };
  } catch (err) {
    return { id, error: err.message };
  }
}

// Synchronous stdin line reader.
// Bun's async stdin APIs (stream, events, for-await) buffer pipe data and
// only deliver it on EOF, which breaks the JSON-RPC protocol when the Go
// host keeps the pipe open. Using fs.readSync on fd 0 avoids this issue.
// StringDecoder handles multi-byte UTF-8 characters split across reads.
const _stdinBuf = Buffer.alloc(65536);
const _stdinDecoder = new StringDecoder('utf8');
let _stdinLeftover = '';

function readLine() {
  while (true) {
    const idx = _stdinLeftover.indexOf('\n');
    if (idx !== -1) {
      const line = _stdinLeftover.slice(0, idx).trim();
      _stdinLeftover = _stdinLeftover.slice(idx + 1);
      if (line) return line;
      continue;
    }
    let n;
    try {
      n = readSync(0, _stdinBuf);
    } catch (err) {
      throw new Error('stdin read error: ' + (err.message || err));
    }
    if (n === 0) return null; // EOF
    _stdinLeftover += _stdinDecoder.write(_stdinBuf.subarray(0, n));
  }
}

// Main loop - read JSON lines from stdin synchronously, handle async
async function main() {
  while (true) {
    const line = readLine();
    if (line === null) break;

    try {
      const req = JSON.parse(line);
      const resp = await handleRequest(req);
      sendResponse(resp);
    } catch (err) {
      // Invalid JSON, send error response
      sendResponse({ id: 0, error: `Invalid request: ${err.message}` });
    }
  }
}

main().then(() => {
  process.exit(0);
}).catch(err => {
  process.stderr.write('[host.js] Fatal: ' + err.message + '\n');
  process.exit(1);
});
