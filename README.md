# Nupi

**Open-source orchestrator for AI coding tools. Voice and text. One layer to run them all.**

---

Nupi sits on top of your AI coding assistants (Claude Code, Codex, Gemini, and others) and lets you coordinate, monitor, and control them from a single place. With your voice, from your terminal, or from your phone.

Instead of managing a grid of terminals and manually copying context between tools, you get one orchestrator that keeps everything in sync.

```
You (voice / text / mobile)
        │
        ▼
   ┌─────────┐
   │  Nupi   │  orchestrator + memory + voice pipeline
   │ (nupid) │
   └────┬────┘
        │
   ┌────┼──────────────┐
   │    │              │
   ▼    ▼              ▼
Claude  Codex        Gemini
 Code                 CLI
```

## Why

AI-assisted development today means juggling multiple CLI tools, terminal sessions, and context windows. You're switching between Claude Code and Codex, copying outputs between them, manually triggering code reviews, and losing track of what each agent is doing.

Nupi fixes this:

- **Multi-tool orchestration** – Claude Code implements a feature, Codex reviews it, Nupi handles the hand-off. Define the workflow once.
- **Voice-first interface** – talk to your tools. Nupi transcribes, understands intent, and dispatches commands. Built-in STT, TTS, and VAD.
- **Project memory** – Nupi remembers your projects, decisions, and context across sessions. Nothing is lost.
- **Remote access** – leave your desk. Get status reports on a walk, discuss architecture from your car, intervene from your phone when a decision is needed.
- **Scheduled tasks** – ask Nupi to report every 30 minutes, and it will. Automate recurring checks, reviews, and syncs.
- **Plugin architecture** – event-bus-driven. Every component broadcasts and reacts. Extend Nupi with custom adapters and plugins.

## Architecture

Nupi follows a daemon/client model, inspired by Docker:

| Component | Language | Role |
|-----------|----------|------|
| `nupid` | Go | Core daemon: session manager, event bus, content pipeline, conversation engine, audio stack, adapter runtime |
| `nupi` | Go | CLI client: session management, adapter configuration, voice control |
| Desktop app | Tauri (Rust + React) | Native GUI for macOS, Windows, Linux |
| Mobile app | – | Remote access and voice control (in development) |

### How it works

```
┌─────────────────────────────────────────────────────────┐
│                    DAEMON (nupid)                        │
│                                                         │
│  Session Manager (PTY)  ←→  Event Bus  ←→  Plugins      │
│  Content Pipeline       ←→  Conversation Engine          │
│  Audio Ingress/Egress   ←→  STT / TTS / VAD Adapters    │
│  Intent Router          ←→  AI Adapter                   │
│                                                         │
│  APIs: Unix socket (CLI) + WebSocket/HTTP (Desktop/Web)  │
└──────────┬──────────────────────────────┬───────────────┘
           │                              │
           │ Unix Socket                  │ WebSocket/HTTP
           │                              │
 ┌─────────▼──────────┐       ┌───────────▼───────────┐
 │   CLI (nupi)       │       │  Desktop / Mobile App  │
 └────────────────────┘       └───────────────────────┘
```

### Plugin Ecosystem

Nupi adapters are pluggable modules connected via the **Nupi Adapter Protocol (NAP)**:

| Plugin | Type | Description |
|--------|------|-------------|
| [plugin-ai-mixed-vercel-ai](https://github.com/nupi-ai/plugin-ai-mixed-vercel-ai) | AI | Multi-provider adapter (OpenAI, Anthropic, Gemini, Ollama) |
| [plugin-stt-local-whisper](https://github.com/nupi-ai/plugin-stt-local-whisper) | STT | Local speech-to-text via whisper.cpp, no cloud dependency |
| [plugin-tts-remote-elevenlabs](https://github.com/nupi-ai/plugin-tts-remote-elevenlabs) | TTS | Text-to-speech via ElevenLabs API |
| [plugin-vad-local-silero](https://github.com/nupi-ai/plugin-vad-local-silero) | VAD | Local voice activity detection (Silero VAD + ONNX) |
| [plugin-tool-handler-claude-code](https://github.com/nupi-ai/plugin-tool-handler-claude-code) | Tool | Claude Code session detection and management |
| [plugin-pipeline-cleaner-default](https://github.com/nupi-ai/plugin-pipeline-cleaner-default) | Pipeline | Output normalization before conversation engine |

## Quick Start

```bash
# Build CLI and daemon
make cli daemon
make download-bun
make install

# Start the daemon
nupid

# In another terminal
nupi run claude
nupi list
nupi attach <session-id>
```

### Desktop App

```bash
make app   # build
make dev   # development mode
```

### Testing

```bash
go test ./...          # all tests
go test -race ./...    # with race detection
```

## Configuration

All configuration is stored in SQLite at `~/.nupi/instances/default/config.db`.

| Path | Purpose |
|------|---------|
| `~/.nupi/instances/default/config.db` | Main configuration store |
| `~/.nupi/instances/default/nupi.sock` | Unix socket (daemon ↔ CLI) |
| `~/.nupi/instances/default/port` | HTTP/WebSocket port |
| `~/.nupi/bin/` | Installed binaries (`nupi`, `nupid`, `bun`) |

Key environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `NUPI_HOME` | `~/.nupi` | Nupi home directory |
| `NUPI_INSTANCE` | `default` | Instance name |
| `NUPI_JS_RUNTIME` | – | Path to Bun runtime for JS plugins |

Manage everything via CLI: `nupi config`, `nupi adapters`, `nupi prompts`.

## Project Status

Nupi is under active development and used daily for production work.

| Component | Status |
|-----------|--------|
| Daemon (`nupid`) | In development, functional |
| CLI (`nupi`) | In development, functional |
| Desktop App (Tauri) | In development |
| Plugin System (NAP) | In development |
| Mobile App | Early stage |

**Platforms:** macOS, Linux, Windows

## Contributing

Contributions, feedback, and ideas are welcome. This is an early-stage project, expect breaking changes and rapid iteration.

## License

[MIT](LICENSE)
