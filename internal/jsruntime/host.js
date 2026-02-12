#!/usr/bin/env bun
// host.js - Bun host script for Nupi JS runtime
// Receives JSON-RPC commands via a Unix domain socket with 4-byte
// length-prefixed framing.  Responds on the same connection.
// Supports: loadPlugin, call, shutdown

const { createConnection } = require('node:net');
const { readFileSync } = require('node:fs');

// Redirect console.log/warn/error to stderr to prevent plugin logging
// from corrupting the IPC protocol
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

// Frame header size: 4 bytes (uint32 big-endian payload length)
const HEADER_SIZE = 4;
// Maximum payload: 16 MB
const MAX_PAYLOAD = 16 * 1024 * 1024;

// Write a length-prefixed frame to the socket.
function writeFrame(socket, data) {
  const payload = Buffer.from(data, 'utf8');
  const header = Buffer.alloc(HEADER_SIZE);
  header.writeUInt32BE(payload.length, 0);
  socket.write(header);
  socket.write(payload);
}

// Send a JSON-RPC response over the IPC socket.
function sendResponse(socket, response) {
  writeFrame(socket, JSON.stringify(response));
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
    // Read file content synchronously to avoid Bun async buffering issues
    const source = readFileSync(path, 'utf8');

    // Create a module-like environment
    const exports = {};
    const module = { exports };

    // Execute the plugin code in a sandboxed function scope.
    // This is the established plugin loading pattern - plugins are trusted
    // user-installed files loaded from the plugins directory.
    const pluginLoader = Function('module', 'exports', 'require', source);
    pluginLoader(module, exports, (id) => {
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
        // Return result - the socket close will terminate the process
        return { id, result: { ok: true }, _shutdown: true };
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

// ---------------------------------------------------------------------------
// IPC frame parser - event-driven, reads length-prefixed frames from socket
// ---------------------------------------------------------------------------

function createFrameParser(onFrame) {
  let buf = Buffer.alloc(0);

  return function processData(chunk) {
    buf = Buffer.concat([buf, chunk]);

    while (buf.length >= HEADER_SIZE) {
      const payloadLen = buf.readUInt32BE(0);

      if (payloadLen > MAX_PAYLOAD) {
        process.stderr.write(`[host.js] Fatal: frame too large (${payloadLen} > ${MAX_PAYLOAD}), protocol desync\n`);
        process.exit(1);
      }

      const frameSize = HEADER_SIZE + payloadLen;
      if (buf.length < frameSize) {
        break; // Need more data
      }

      const payload = buf.subarray(HEADER_SIZE, frameSize);
      buf = buf.subarray(frameSize);

      onFrame(payload);
    }
  };
}

// ---------------------------------------------------------------------------
// Main - connect to IPC socket and process requests
// ---------------------------------------------------------------------------

function main() {
  const socketPath = process.env.NUPI_IPC_SOCKET;
  if (!socketPath) {
    process.stderr.write('[host.js] Fatal: NUPI_IPC_SOCKET environment variable not set\n');
    process.exit(1);
  }

  const socket = createConnection({ path: socketPath }, () => {
    // Connected - ready to receive frames
  });

  // Queue to serialise request handling (one at a time).
  let processing = Promise.resolve();

  const parser = createFrameParser((payload) => {
    const text = payload.toString('utf8');
    processing = processing.then(async () => {
      try {
        const req = JSON.parse(text);
        const resp = await handleRequest(req);
        const isShutdown = resp._shutdown;
        delete resp._shutdown;
        sendResponse(socket, resp);
        if (isShutdown) {
          socket.end(() => process.exit(0));
        }
      } catch (err) {
        sendResponse(socket, { id: 0, error: `Invalid request: ${err.message}` });
      }
    });
  });

  socket.on('data', parser);

  socket.on('error', (err) => {
    process.stderr.write(`[host.js] Socket error: ${err.message}\n`);
    process.exit(1);
  });

  socket.on('close', () => {
    process.exit(0);
  });
}

main();
