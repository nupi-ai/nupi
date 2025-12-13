#!/usr/bin/env bun
// host.js - Bun host script for Nupi JS runtime
// Receives JSON-RPC commands via stdin, responds via stdout
// Supports: loadPlugin, call, shutdown

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
    // Read file content
    const file = Bun.file(path);
    const source = await file.text();

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
      hasSummarize: typeof pluginExports.summarize === 'function',
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

// Main loop - read JSON lines from stdin
const decoder = new TextDecoder();
const encoder = new TextEncoder();

async function main() {
  const reader = Bun.stdin.stream().getReader();
  let buffer = '';

  while (true) {
    const { done, value } = await reader.read();

    if (done) {
      // stdin closed, exit gracefully
      break;
    }

    buffer += decoder.decode(value, { stream: true });

    // Process complete lines
    let newlineIdx;
    while ((newlineIdx = buffer.indexOf('\n')) !== -1) {
      const line = buffer.slice(0, newlineIdx).trim();
      buffer = buffer.slice(newlineIdx + 1);

      if (!line) continue;

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
}

main().catch(err => {
  process.stderr.write('[host.js] Fatal: ' + err.message + '\n');
  process.exit(1);
});
